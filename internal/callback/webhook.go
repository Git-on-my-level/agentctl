package callback

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	maxWebhookBody = 1 << 20
	maxAckBody     = 16 << 10
	// AttemptEnvelopeTTL bounds freshness independently of the subscription's
	// overall expiry. Receivers retain replay state through this window.
	AttemptEnvelopeTTL = 5 * time.Minute
)

// Envelope is the bounded callback document sent to all non-parent
// destinations. Event is kept as raw JSON to preserve the event projection.
type Envelope struct {
	SchemaVersion  int             `json:"schema_version"`
	DeliveryID     string          `json:"delivery_id"`
	SubscriptionID string          `json:"subscription_id"`
	EventID        string          `json:"event_id"`
	EventDedupeKey string          `json:"event_dedupe_key"`
	Attempt        int             `json:"attempt"`
	SentAt         time.Time       `json:"sent_at"`
	ExpiresAt      time.Time       `json:"expires_at"`
	Nonce          string          `json:"nonce"`
	Event          json.RawMessage `json:"event"`
}

func NewEnvelope(deliveryID, subscriptionID, eventID, dedupeKey, nonce string, attempt int, sentAt, expiresAt time.Time, event any) (Envelope, error) {
	if deliveryID == "" || subscriptionID == "" || eventID == "" || dedupeKey == "" || nonce == "" {
		return Envelope{}, errors.New("delivery, subscription, event, dedupe key, and nonce are required")
	}
	if attempt < 1 || expiresAt.Before(sentAt) {
		return Envelope{}, errors.New("invalid attempt or expiry")
	}
	b, err := CanonicalJSON(event)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{SchemaVersion: 1, DeliveryID: deliveryID, SubscriptionID: subscriptionID, EventID: eventID, EventDedupeKey: dedupeKey, Attempt: attempt, SentAt: sentAt.UTC(), ExpiresAt: expiresAt.UTC(), Nonce: nonce, Event: b}, nil
}

// ReissueEnvelope creates the per-attempt wire document from a durable base
// envelope. Delivery/event identity remains stable while attempt number,
// timestamps, expiry, and nonce are fresh. The durable delivery expiry is a
// hard upper bound, so a retry cannot extend a subscription past its limit.
func ReissueEnvelope(stored Envelope, attempt int, sentAt, deliveryExpiresAt time.Time) (Envelope, error) {
	if err := stored.Validate(time.Time{}); err != nil {
		return Envelope{}, err
	}
	if attempt < 1 {
		return Envelope{}, errors.New("attempt must be positive")
	}
	if sentAt.IsZero() {
		sentAt = time.Now().UTC()
	} else {
		sentAt = sentAt.UTC()
	}
	if !stored.SentAt.IsZero() && !sentAt.After(stored.SentAt) {
		// A coarse/frozen clock must not produce two scheduled attempts with
		// the same sent_at. The nanosecond tie-breaker remains bounded by the
		// durable delivery expiry below.
		sentAt = stored.SentAt.UTC().Add(time.Nanosecond)
	}
	deliveryExpiresAt = deliveryExpiresAt.UTC()
	if deliveryExpiresAt.IsZero() {
		deliveryExpiresAt = stored.ExpiresAt.UTC()
	}
	if !deliveryExpiresAt.After(sentAt) {
		return Envelope{}, errors.New("delivery is expired")
	}
	expiresAt := sentAt.Add(AttemptEnvelopeTTL)
	if deliveryExpiresAt.Before(expiresAt) {
		expiresAt = deliveryExpiresAt
	}
	nonce, err := NewNonce()
	if err != nil {
		return Envelope{}, err
	}
	// Canonicalize the stored event again so malformed or noncanonical durable
	// payloads fail closed before a new attempt is persisted or sent.
	event, err := CanonicalJSON(stored.Event)
	if err != nil {
		return Envelope{}, err
	}
	result := stored
	result.Attempt = attempt
	result.SentAt = sentAt
	result.ExpiresAt = expiresAt
	result.Nonce = nonce
	result.Event = event
	return result, nil
}

// Reissue is the method form of ReissueEnvelope for transport implementations
// that retain the base envelope as a value.
func (e Envelope) Reissue(attempt int, sentAt, deliveryExpiresAt time.Time) (Envelope, error) {
	return ReissueEnvelope(e, attempt, sentAt, deliveryExpiresAt)
}

// NewNonce returns an opaque cryptographically random callback nonce. It is
// not a credential and is safe to persist with the in-flight envelope.
func NewNonce() (string, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate callback nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func (e Envelope) Validate(now time.Time) error {
	if e.SchemaVersion != 1 || e.DeliveryID == "" || e.SubscriptionID == "" || e.EventID == "" || e.EventDedupeKey == "" || e.Nonce == "" || e.Attempt < 1 || len(e.Event) == 0 || len(e.Event) > maxWebhookBody {
		return errors.New("invalid callback envelope")
	}
	if e.ExpiresAt.Before(e.SentAt) || (!now.IsZero() && now.After(e.ExpiresAt)) {
		return errors.New("callback envelope expired")
	}
	return nil
}

func (e Envelope) CanonicalBody() ([]byte, error) {
	// Round-trip through generic JSON to ensure field ordering and time format
	// are canonical and to reject unsupported raw event values.
	raw, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	var value any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	return CanonicalJSON(value)
}

// CanonicalRequest is the exact byte-level signing projection. The target is
// the normalized path only; query strings are refused to avoid ambiguous
// intermediaries and credential leakage.
type CanonicalRequest struct {
	Method         string
	Path           string
	ContentType    string
	BodyDigest     string
	SentAt         string
	ExpiresAt      string
	Nonce          string
	SubscriptionID string
	DeliveryID     string
}

func (r CanonicalRequest) Bytes() ([]byte, error) {
	method := strings.ToUpper(strings.TrimSpace(r.Method))
	if method == "" || r.Path == "" || r.ContentType == "" || r.BodyDigest == "" || r.SentAt == "" || r.ExpiresAt == "" || r.Nonce == "" || r.SubscriptionID == "" || r.DeliveryID == "" {
		return nil, errors.New("incomplete canonical request")
	}
	p, err := normalizePath(r.Path)
	if err != nil {
		return nil, err
	}
	parts := []string{"agentctl-webhook-v1", method, p, strings.TrimSpace(r.ContentType), strings.TrimSpace(r.BodyDigest), strings.TrimSpace(r.SentAt), strings.TrimSpace(r.ExpiresAt), r.Nonce, r.SubscriptionID, r.DeliveryID}
	return []byte(strings.Join(parts, "\n")), nil
}

func normalizePath(path string) (string, error) {
	if path == "" || !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\r\n") {
		return "", errors.New("invalid request path")
	}
	if strings.Contains(path, "?") || strings.Contains(path, "#") {
		return "", errors.New("query and fragment are not signed")
	}
	// url.URL.Path is decoded, so avoid silently changing escaped octets.
	u, err := url.ParseRequestURI(path)
	if err != nil || u.Path == "" {
		return "", errors.New("invalid request path")
	}
	segments := strings.Split(u.Path, "/")
	stack := make([]string, 0, len(segments))
	for _, seg := range segments {
		switch seg {
		case "", ".":
			continue
		case "..":
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		default:
			stack = append(stack, seg)
		}
	}
	if len(stack) == 0 {
		return "/", nil
	}
	return "/" + strings.Join(stack, "/"), nil
}

// Sign computes HMAC-SHA256 over the frozen canonical request profile and
// returns a versioned base64url value suitable for X-Agentctl-Signature.
func Sign(key []byte, request CanonicalRequest) (string, error) {
	if len(key) == 0 {
		return "", errors.New("signing key is empty")
	}
	b, err := request.Bytes()
	if err != nil {
		return "", err
	}
	h := hmac.New(sha256.New, key)
	_, _ = h.Write(b)
	return "hmac-sha256=" + base64.RawURLEncoding.EncodeToString(h.Sum(nil)), nil
}

func VerifySignature(key []byte, request CanonicalRequest, signature string) error {
	expected, err := Sign(key, request)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(expected), []byte(signature)) != 1 {
		return errors.New("invalid webhook signature")
	}
	return nil
}

// BuildWebhookRequest creates a request with redirects disabled by the
// transport returned from WebhookHTTPClient. It does not transmit anything.
func BuildWebhookRequest(e Envelope, endpoint string, key []byte, now time.Time) (*http.Request, error) {
	return BuildWebhookRequestWithAllowedHosts(e, endpoint, key, now, nil)
}

// BuildWebhookRequestWithAllowedHosts is the destination-aware variant used
// for explicit local/Tailnet callbacks.
func BuildWebhookRequestWithAllowedHosts(e Envelope, endpoint string, key []byte, now time.Time, allowedHosts []string) (*http.Request, error) {
	if err := e.Validate(now); err != nil {
		return nil, err
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid webhook endpoint")
	}
	if err := ValidateWebhookURL(endpoint, allowedHosts); err != nil {
		return nil, err
	}
	body, err := e.CanonicalBody()
	if err != nil {
		return nil, err
	}
	if len(body) > maxWebhookBody {
		return nil, errors.New("callback envelope exceeds size limit")
	}
	digest := sha256.Sum256(body)
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	reqMeta := CanonicalRequest{Method: http.MethodPost, Path: path, ContentType: "application/json", BodyDigest: "sha256:" + hex.EncodeToString(digest[:]), SentAt: e.SentAt.UTC().Format(time.RFC3339Nano), ExpiresAt: e.ExpiresAt.UTC().Format(time.RFC3339Nano), Nonce: e.Nonce, SubscriptionID: e.SubscriptionID, DeliveryID: e.DeliveryID}
	signature, err := Sign(key, reqMeta)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agentctl-Signature", signature)
	req.Header.Set("X-Agentctl-Body-Digest", reqMeta.BodyDigest)
	req.Header.Set("X-Agentctl-Sent-At", reqMeta.SentAt)
	req.Header.Set("X-Agentctl-Expires-At", reqMeta.ExpiresAt)
	req.Header.Set("X-Agentctl-Nonce", e.Nonce)
	req.Header.Set("X-Agentctl-Subscription-ID", e.SubscriptionID)
	req.Header.Set("X-Agentctl-Delivery-ID", e.DeliveryID)
	return req, nil
}

// WebhookHTTPClient returns a redirect-free client for callers that have
// already selected and validated a destination. For destination-aware sends,
// use NewSafeWebhookClient so every DNS result is checked at connect time.
func WebhookHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &http.Client{Timeout: timeout, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return ErrRedirectRefused }}
}

// IPResolver is injectable so destination planning and connect-time DNS
// checks can be tested without external network access.
type IPResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

// ResolverFunc adapts a function to IPResolver.
type ResolverFunc func(context.Context, string) ([]net.IPAddr, error)

func (f ResolverFunc) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return f(ctx, host)
}

// NewSafeWebhookClient constructs a redirect-free client whose dial path
// re-resolves and validates every candidate IP. Public DNS can therefore not
// rebind a previously planned host to a loopback, link-local, RFC1918, ULA,
// or cloud metadata address. An exact host allowlist entry is required for
// intentional private/Tailnet destinations.
func NewSafeWebhookClient(endpoint string, allowedHosts []string, timeout time.Duration, resolver IPResolver) (*http.Client, error) {
	if err := ValidateWebhookURL(endpoint, allowedHosts); err != nil {
		return nil, err
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // do not delegate destination policy to an ambient proxy
	transport.DialContext = (&safeDialer{resolver: resolver, expectedHost: strings.ToLower(u.Hostname()), allowPrivate: exactHostAllowlisted(u, allowedHosts)}).DialContext
	transport.TLSHandshakeTimeout = 10 * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return ErrRedirectRefused }}, nil
}

type safeDialer struct {
	resolver     IPResolver
	expectedHost string
	allowPrivate bool
	dialer       net.Dialer
}

func (d *safeDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if d.expectedHost != "" && !strings.EqualFold(host, d.expectedHost) {
		return nil, fmt.Errorf("webhook dial host changed from planned destination")
	}
	addrs, err := d.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("webhook destination has no resolved addresses")
	}
	for _, candidate := range addrs {
		if isBlockedWebhookIP(candidate.IP) && !d.allowPrivate {
			return nil, fmt.Errorf("webhook destination resolved to private or local address")
		}
	}
	var lastErr error
	for _, candidate := range addrs {
		ip := candidate.IP.String()
		if candidate.Zone != "" {
			ip += "%" + candidate.Zone
		}
		conn, dialErr := d.dialer.DialContext(ctx, network, net.JoinHostPort(ip, port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	if lastErr == nil {
		lastErr = errors.New("webhook destination could not connect")
	}
	return nil, lastErr
}

func exactHostAllowlisted(u *url.URL, allowed []string) bool {
	host := strings.ToLower(u.Hostname())
	for _, value := range allowed {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == host || value == strings.ToLower(u.Host) {
			return true
		}
	}
	return false
}

// Acknowledgement is the only response body accepted as delivery proof.
type Acknowledgement struct {
	DeliveryID     string    `json:"delivery_id"`
	EventDedupeKey string    `json:"event_dedupe_key"`
	ReceiverID     string    `json:"receiver_id"`
	AcknowledgedAt time.Time `json:"acknowledged_at"`
}

func ValidateAcknowledgement(body []byte, e Envelope, now time.Time) (Acknowledgement, error) {
	if len(body) == 0 || len(body) > maxAckBody {
		return Acknowledgement{}, ErrInvalidAck
	}
	var ack Acknowledgement
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ack); err != nil {
		return Acknowledgement{}, fmt.Errorf("%w: %v", ErrInvalidAck, err)
	}
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return Acknowledgement{}, ErrInvalidAck
	} else if !errors.Is(err, io.EOF) {
		return Acknowledgement{}, fmt.Errorf("%w: trailing JSON: %v", ErrInvalidAck, err)
	}
	if ack.DeliveryID != e.DeliveryID || ack.EventDedupeKey != e.EventDedupeKey || ack.ReceiverID == "" || ack.AcknowledgedAt.IsZero() {
		return Acknowledgement{}, ErrInvalidAck
	}
	if !now.IsZero() && ack.AcknowledgedAt.After(now.Add(5*time.Minute)) {
		return Acknowledgement{}, ErrInvalidAck
	}
	return ack, nil
}

func IsAcknowledged(status int, body []byte, e Envelope, now time.Time) (Acknowledgement, error) {
	if status < 200 || status > 299 {
		return Acknowledgement{}, fmt.Errorf("HTTP %d is not an acknowledgement", status)
	}
	return ValidateAcknowledgement(body, e, now)
}

// ReplayGuard is an in-memory reference implementation of durable receiver
// replay state. Production stores can implement the same CheckAndRecord
// semantics transactionally.
type ReplayGuard struct {
	mu      sync.Mutex
	entries map[string]replayEntry
	Window  time.Duration
}

type replayEntry struct {
	delivery   string
	bodyDigest string
	expires    time.Time
	ack        *Acknowledgement
}

// ReplayResult tells a receiver whether it may process a new request. An
// exact duplicate with a cached acknowledgement is safe to answer again;
// callers must not re-run the event side effect.
type ReplayResult struct {
	Duplicate       bool
	Acknowledgement *Acknowledgement
}

func NewReplayGuard(window time.Duration) *ReplayGuard {
	if window <= 0 {
		window = 10 * time.Minute
	}
	return &ReplayGuard{entries: make(map[string]replayEntry), Window: window}
}

// CheckAndRecord atomically rejects a previously seen nonce, while retaining
// entries until expiry plus the configured skew/retry window.
func (g *ReplayGuard) CheckAndRecord(nonce, deliveryID string, expiresAt, now time.Time) error {
	if g == nil || nonce == "" || deliveryID == "" {
		return ErrReplay
	}
	if now.IsZero() {
		now = time.Now()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for key, entry := range g.entries {
		if now.After(entry.expires.Add(g.Window)) {
			delete(g.entries, key)
		}
	}
	if _, ok := g.entries[nonce]; ok {
		return ErrReplay
	}
	if expiresAt.Before(now) || expiresAt.After(now.Add(g.Window)) {
		return ErrReplay
	}
	g.entries[nonce] = replayEntry{delivery: deliveryID, expires: expiresAt}
	return nil
}

// CheckEnvelope atomically records a new nonce/body pair. Reusing a nonce for
// another body or delivery is rejected. Reusing the exact pair returns the
// cached acknowledgement when one exists, allowing a sender that crashed
// after receiver processing to converge without running the side effect twice.
func (g *ReplayGuard) CheckEnvelope(e Envelope, body []byte, now time.Time) (ReplayResult, error) {
	if g == nil || e.Nonce == "" || e.DeliveryID == "" {
		return ReplayResult{}, ErrReplay
	}
	if len(body) == 0 || len(body) > maxWebhookBody {
		return ReplayResult{}, ErrReplay
	}
	canonicalBody, err := e.CanonicalBody()
	if err != nil || !bytes.Equal(body, canonicalBody) {
		return ReplayResult{}, ErrReplay
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	digest := bodyDigest(body)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cleanup(now)
	if existing, ok := g.entries[e.Nonce]; ok {
		if existing.delivery != e.DeliveryID || existing.bodyDigest != digest {
			return ReplayResult{}, ErrReplay
		}
		result := ReplayResult{Duplicate: true}
		if existing.ack != nil {
			ack := *existing.ack
			result.Acknowledgement = &ack
		}
		return result, nil
	}
	if err := e.Validate(now); err != nil {
		return ReplayResult{}, err
	}
	g.entries[e.Nonce] = replayEntry{delivery: e.DeliveryID, bodyDigest: digest, expires: e.ExpiresAt}
	return ReplayResult{}, nil
}

// RecordAcknowledgement attaches a strict acknowledgement to a previously
// accepted request. It is idempotent for the same metadata and rejects any
// conflicting acknowledgement or request body.
func (g *ReplayGuard) RecordAcknowledgement(e Envelope, body []byte, ack Acknowledgement, now time.Time) error {
	if g == nil {
		return ErrReplay
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if _, err := ValidateAcknowledgement(mustAcknowledgementJSON(ack), e, now); err != nil {
		return err
	}
	digest := bodyDigest(body)
	canonicalBody, err := e.CanonicalBody()
	if err != nil || !bytes.Equal(body, canonicalBody) {
		return ErrReplay
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cleanup(now)
	entry, ok := g.entries[e.Nonce]
	if !ok || entry.delivery != e.DeliveryID || entry.bodyDigest != digest {
		return ErrReplay
	}
	if entry.ack != nil {
		if *entry.ack != ack {
			return ErrReplay
		}
		return nil
	}
	copyAck := ack
	entry.ack = &copyAck
	g.entries[e.Nonce] = entry
	return nil
}

// Check is a concise alias for CheckEnvelope used by receiver adapters.
func (g *ReplayGuard) Check(e Envelope, body []byte, now time.Time) (ReplayResult, error) {
	return g.CheckEnvelope(e, body, now)
}

// RecordAck is a concise alias for RecordAcknowledgement.
func (g *ReplayGuard) RecordAck(e Envelope, body []byte, ack Acknowledgement, now time.Time) error {
	return g.RecordAcknowledgement(e, body, ack, now)
}

func (g *ReplayGuard) cleanup(now time.Time) {
	for key, entry := range g.entries {
		if now.After(entry.expires.Add(g.Window)) {
			delete(g.entries, key)
		}
	}
}

func bodyDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func mustAcknowledgementJSON(ack Acknowledgement) []byte {
	b, _ := json.Marshal(ack)
	return b
}

// ValidateWebhookURL performs destination planning checks without DNS or
// network I/O. Host allowlists are exact (case-insensitive) and are required
// for non-public/local names when provided by the caller.
func ValidateWebhookURL(raw string, allowedHosts []string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return fmt.Errorf("%w: webhook URL must be http(s), host-only, and query-free", ErrInvalidDestination)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return fmt.Errorf("%w: empty webhook host", ErrInvalidDestination)
	}
	exactAllowed := exactHostAllowlisted(u, allowedHosts)
	if len(allowedHosts) > 0 && !exactAllowed {
		return fmt.Errorf("%w: webhook host is not allowlisted", ErrInvalidDestination)
	}
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedWebhookIP(ip) && !exactAllowed {
			return fmt.Errorf("%w: local or metadata address refused", ErrInvalidDestination)
		}
	}
	if u.Scheme == "http" {
		// Plaintext callbacks are only permitted for an explicitly allowlisted
		// loopback/private/Tailnet destination. Public webhook delivery always
		// requires HTTPS even when an operator supplied an allowlist entry.
		if !exactAllowed || !isLocalOrPrivateWebhookHost(host) {
			return fmt.Errorf("%w: public webhook destinations require HTTPS", ErrInvalidDestination)
		}
	}
	return nil
}

func isLocalOrPrivateWebhookHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return isBlockedWebhookIP(ip)
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".home.arpa") || strings.HasSuffix(host, ".ts.net") || strings.HasSuffix(host, ".tailnet") {
		return true
	}
	return false
}

func isBlockedWebhookIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsPrivate() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		// RFC 1918 plus 100.64/10 and cloud metadata 169.254.169.254.
		return v4[0] == 10 || (v4[0] == 172 && v4[1] >= 16 && v4[1] <= 31) || (v4[0] == 192 && v4[1] == 168) || (v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127) || (v4[0] == 169 && v4[1] == 254 && v4[2] == 169 && v4[3] == 254)
	}
	return false
}

// SortedAllowedHosts gives deterministic plan output without exposing any
// credential material.
func SortedAllowedHosts(hosts []string) []string {
	result := append([]string(nil), hosts...)
	sort.Strings(result)
	return result
}
