package callback

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testEnvelope(t *testing.T) Envelope {
	t.Helper()
	sent := time.Date(2026, 8, 10, 18, 5, 3, 0, time.UTC)
	e, err := NewEnvelope("delivery-test", "sub-test", "event-test", "sha256:event", "nonce-test", 3, sent, sent.Add(5*time.Minute), map[string]any{"state": "completed"})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestWebhookSignatureFixtureAndAck(t *testing.T) {
	e := testEnvelope(t)
	req, err := BuildWebhookRequest(e, "https://example.com/callback", []byte("secret"), e.SentAt)
	if err != nil {
		t.Fatal(err)
	}
	sig := req.Header.Get("X-Agentctl-Signature")
	if !strings.HasPrefix(sig, "hmac-sha256=") {
		t.Fatalf("signature = %q", sig)
	}
	if err := VerifySignature([]byte("secret"), CanonicalRequest{Method: "POST", Path: "/callback", ContentType: "application/json", BodyDigest: req.Header.Get("X-Agentctl-Body-Digest"), SentAt: e.SentAt.Format(time.RFC3339Nano), ExpiresAt: e.ExpiresAt.Format(time.RFC3339Nano), Nonce: e.Nonce, SubscriptionID: e.SubscriptionID, DeliveryID: e.DeliveryID}, sig); err != nil {
		t.Fatal(err)
	}
	body := `{"delivery_id":"delivery-test","event_dedupe_key":"sha256:event","receiver_id":"receiver-1","acknowledged_at":"2026-08-10T18:05:04Z"}`
	if _, err := ValidateAcknowledgement([]byte(body), e, e.SentAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestReissueEnvelopeRefreshesScheduledAttempts(t *testing.T) {
	base := testEnvelope(t)
	now := base.SentAt.Add(time.Minute)
	first, err := ReissueEnvelope(base, 1, now, base.SentAt.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	second, err := ReissueEnvelope(first, 2, now.Add(time.Second), base.SentAt.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if first.DeliveryID != second.DeliveryID || first.EventID != second.EventID || first.EventDedupeKey != second.EventDedupeKey || first.Nonce == second.Nonce || first.Attempt != 1 || second.Attempt != 2 || !second.SentAt.After(first.SentAt) {
		t.Fatalf("attempt identity/metadata mismatch: first=%+v second=%+v", first, second)
	}
	if first.ExpiresAt.After(first.SentAt.Add(AttemptEnvelopeTTL)) || second.ExpiresAt.After(second.SentAt.Add(AttemptEnvelopeTTL)) {
		t.Fatal("attempt expiry exceeded freshness bound")
	}
}

func TestReplayGuardAndRetryClasses(t *testing.T) {
	e := testEnvelope(t)
	g := NewReplayGuard(10 * time.Minute)
	if err := g.CheckAndRecord(e.Nonce, e.DeliveryID, e.ExpiresAt, e.SentAt); err != nil {
		t.Fatal(err)
	}
	if err := g.CheckAndRecord(e.Nonce, e.DeliveryID, e.ExpiresAt, e.SentAt); err != ErrReplay {
		t.Fatalf("second nonce accepted: %v", err)
	}
	if ClassifyHTTP(401, nil) != RetryPaused || ClassifyHTTP(404, nil) != RetryDeadLetter || ClassifyHTTP(503, nil) != RetryTransient || ClassifyHTTP(204, nil) != RetryAcknowledged {
		t.Fatal("retry classification regression")
	}
}

func TestReplayGuardReturnsCachedAckAndRejectsConflictingReuse(t *testing.T) {
	e := testEnvelope(t)
	body, err := e.CanonicalBody()
	if err != nil {
		t.Fatal(err)
	}
	guard := NewReplayGuard(10 * time.Minute)
	first, err := guard.CheckEnvelope(e, body, e.SentAt)
	if err != nil || first.Duplicate {
		t.Fatalf("first replay decision=%+v err=%v", first, err)
	}
	ack := Acknowledgement{DeliveryID: e.DeliveryID, EventDedupeKey: e.EventDedupeKey, ReceiverID: "receiver-1", AcknowledgedAt: e.SentAt.Add(time.Second)}
	if err := guard.RecordAcknowledgement(e, body, ack, e.SentAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	duplicate, err := guard.CheckEnvelope(e, body, e.SentAt.Add(2*time.Second))
	if err != nil || !duplicate.Duplicate || duplicate.Acknowledgement == nil || *duplicate.Acknowledgement != ack {
		t.Fatalf("duplicate replay decision=%+v err=%v", duplicate, err)
	}
	conflictingBody := append([]byte(nil), body...)
	conflictingBody[len(conflictingBody)-2] ^= 1
	if _, err := guard.CheckEnvelope(e, conflictingBody, e.SentAt.Add(2*time.Second)); err != ErrReplay {
		t.Fatalf("conflicting nonce/body accepted: %v", err)
	}
}

func TestWebhookRejectsRedirectableQueryAndPrivateTarget(t *testing.T) {
	for _, target := range []string{"https://example.com/callback?token=x", "http://127.0.0.1/callback", "https://169.254.169.254/latest"} {
		if err := ValidateWebhookURL(target, nil); err == nil {
			t.Fatalf("accepted unsafe target %q", target)
		}
	}
}

func TestWebhookPlainHTTPRequiresExplicitLocalPrivateAllowlist(t *testing.T) {
	if err := ValidateWebhookURL("http://example.com/callback", nil); err == nil {
		t.Fatal("accepted public plaintext webhook")
	}
	if err := ValidateWebhookURL("http://example.com/callback", []string{"example.com"}); err == nil {
		t.Fatal("allowlist bypassed HTTPS requirement for public host")
	}
	for _, target := range []struct {
		url   string
		hosts []string
	}{{"http://127.0.0.1/callback", []string{"127.0.0.1"}}, {"http://10.1.2.3/callback", []string{"10.1.2.3"}}, {"http://receiver.ts.net/callback", []string{"receiver.ts.net"}}} {
		if err := ValidateWebhookURL(target.url, target.hosts); err != nil {
			t.Fatalf("explicit local/private plaintext target rejected: %s: %v", target.url, err)
		}
	}
}

func TestSafeWebhookDialRejectsDNSRebindingAndIPv6ULA(t *testing.T) {
	resolver := ResolverFunc(func(_ context.Context, host string) ([]net.IPAddr, error) {
		switch host {
		case "rebind.example":
			return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}, {IP: net.ParseIP("127.0.0.1")}}, nil
		case "ula.example":
			return []net.IPAddr{{IP: net.ParseIP("fd00::1")}}, nil
		default:
			return nil, errors.New("unexpected host")
		}
	})
	for _, target := range []struct{ endpoint, address string }{{"https://rebind.example/callback", "rebind.example:443"}, {"https://ula.example/callback", "ula.example:443"}} {
		client, err := NewSafeWebhookClient(target.endpoint, nil, time.Second, resolver)
		if err != nil {
			t.Fatal(err)
		}
		transport := client.Transport.(*http.Transport)
		if _, err := transport.DialContext(context.Background(), "tcp", target.address); err == nil {
			t.Fatalf("accepted unsafe DNS result for %s", target.endpoint)
		}
	}
}

func TestValidateUnixSocketOwnershipTypeAndSymlink(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "callback.sock")
	if err := ValidateUnixSocket(path); err == nil {
		t.Fatal("accepted missing Unix socket")
	}
	if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateUnixSocket(path); err == nil {
		t.Fatal("accepted regular file as Unix socket")
	}
	_ = os.Remove(path)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	defer os.Remove(path)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateUnixSocket(path); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "callback-link.sock")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := ValidateUnixSocket(link); err == nil {
		t.Fatal("accepted symlink Unix socket")
	}
	if err := os.Chmod(path, 0o660); err != nil {
		t.Fatal(err)
	}
	if err := ValidateUnixSocket(path); err == nil {
		t.Fatal("accepted group-accessible Unix socket")
	}
}

func TestUnixUnacknowledgedAndAcknowledgedDelivery(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "callback.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	defer os.Remove(path)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"delivery_id":"delivery-test"}`)
	serverErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer conn.Close()
		buf := make([]byte, len(body))
		_, readErr := conn.Read(buf)
		serverErr <- readErr
	}()
	if err := SendUnixUnacknowledged(context.Background(), path, body); err != nil {
		t.Fatal(err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
	if ClassifyUnixSend(nil, false) != RetryTransient || ClassifyUnixSend(nil, true) != RetryAcknowledged {
		t.Fatal("Unix acknowledgement classification regression")
	}

	// Recreate the listener because the first connection is intentionally
	// unacknowledged and has already been consumed.
	listener.Close()
	_ = os.Remove(path)
	listener, err = net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	e := testEnvelope(t)
	ackErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			ackErr <- acceptErr
			return
		}
		defer conn.Close()
		_, _ = conn.Read(make([]byte, len(body)))
		_, writeErr := conn.Write([]byte(`{"delivery_id":"delivery-test","event_dedupe_key":"sha256:event","receiver_id":"receiver-1","acknowledged_at":"2026-08-10T18:05:04Z"}`))
		ackErr <- writeErr
	}()
	if _, err := SendUnixWithAcknowledgement(context.Background(), path, body, e, e.SentAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := <-ackErr; err != nil {
		t.Fatal(err)
	}
}

func TestCommandIdentityRejectsPathReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "callback-command")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanCommand(Destination{Kind: DestinationCommand, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	destination := Destination{Kind: DestinationCommand, Path: path, CommandIdentity: func() *CommandIdentity { identity := plan.Identity(); return &identity }()}
	if _, err := PlanCommand(destination); err != nil {
		t.Fatal(err)
	}
	backup := path + ".old"
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := PlanCommand(destination); err != ErrExecutableChanged {
		t.Fatalf("replacement identity error = %v", err)
	}
}
