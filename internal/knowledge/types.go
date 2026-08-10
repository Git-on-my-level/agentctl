// Package knowledge implements the source registry, deterministic compiler,
// and verified bundle used by agentctl's context reader.
package knowledge

import "time"

// Mode identifies the shape of a knowledge repository.
type Mode string

const (
	ModeStructured Mode = "structured"
	ModeLoose      Mode = "loose"
	ModeHybrid     Mode = "hybrid"
)

type Provider string

const (
	ProviderGitHub  Provider = "github"
	ProviderForgejo Provider = "forgejo"
	ProviderGeneric Provider = "generic"
)

type Sensitivity string

const (
	SensitivityPublic              Sensitivity = "public"
	SensitivityFleetInternal       Sensitivity = "fleet-internal"
	SensitivityOperatorPrivate     Sensitivity = "operator-private"
	SensitivityProjectConfidential Sensitivity = "project-confidential"
)

// Remote is deliberately a small wrapper around native Git. Credentials are
// never represented here; Git and SSH resolve them from their normal stores.
type Remote struct {
	Provider       Provider `json:"provider" yaml:"provider"`
	URL            string   `json:"url" yaml:"url"`
	WebURL         string   `json:"web_url,omitempty" yaml:"web_url,omitempty"`
	CredentialMode string   `json:"credential_mode" yaml:"credential_mode"`
}

type Overlay struct {
	Kind         string `json:"kind" yaml:"kind"` // in_repo or external
	Path         string `json:"path" yaml:"path"`
	SourceRepoID string `json:"source_repo_id,omitempty" yaml:"source_repo_id,omitempty"`
}

type IngestPolicy struct {
	Include      []string `json:"include" yaml:"include"`
	Exclude      []string `json:"exclude" yaml:"exclude"`
	MaxFileBytes int64    `json:"max_file_bytes" yaml:"max_file_bytes"`
	Encoding     string   `json:"encoding,omitempty" yaml:"encoding,omitempty"`
	Chunking     string   `json:"chunking,omitempty" yaml:"chunking,omitempty"`
	Index        string   `json:"index,omitempty" yaml:"index,omitempty"`
}

// SourceRegistration is the reviewed registration stored by a registry
// repository. Optional fields are represented as values so callers can build
// registrations without a YAML/JSON round trip.
type SourceRegistration struct {
	SchemaVersion      int                 `json:"schema_version" yaml:"schema_version"`
	ID                 string              `json:"id" yaml:"id"`
	Slug               string              `json:"slug" yaml:"slug"`
	Mode               Mode                `json:"mode" yaml:"mode"`
	Remote             Remote              `json:"remote" yaml:"remote"`
	Ref                string              `json:"ref" yaml:"ref"`
	Subpath            string              `json:"subpath" yaml:"subpath"`
	Sensitivity        Sensitivity         `json:"sensitivity" yaml:"sensitivity"`
	StructuredManifest string              `json:"structured_manifest,omitempty" yaml:"structured_manifest,omitempty"`
	Ingest             IngestPolicy        `json:"ingest,omitempty" yaml:"ingest,omitempty"`
	Overlay            Overlay             `json:"overlay,omitempty" yaml:"overlay,omitempty"`
	DefaultScope       map[string][]string `json:"default_scope,omitempty" yaml:"default_scope,omitempty"`
}

// Compatibility aliases keep the schema's terminology discoverable to callers
// without introducing duplicate representations.
type KnowledgeSource = SourceRegistration
type Source = SourceRegistration
type RemoteRegistration = Remote
type IngestionPolicy = IngestPolicy

// SourceRevision is the immutable pin emitted by a compile.
type SourceRevision struct {
	ID            string   `json:"id"`
	Slug          string   `json:"slug"`
	Provider      Provider `json:"provider"`
	RemoteURL     string   `json:"remote_url"`
	Ref           string   `json:"ref"`
	Commit        string   `json:"commit"`
	TreeDigest    string   `json:"tree_digest"`
	ContentDigest string   `json:"content_digest"`
	Subpath       string   `json:"subpath"`
	IngestDigest  string   `json:"ingest_digest"`
	OverlayDigest string   `json:"overlay_digest,omitempty"`
}

type Provenance struct {
	SourceRepoID  string `json:"source_repo_id"`
	SourceCommit  string `json:"source_commit"`
	Path          string `json:"path"`
	StartLine     int    `json:"start_line"`
	EndLine       int    `json:"end_line"`
	ContentDigest string `json:"content_digest"`
}

// Record is the common compiled unit for structured records and loose chunks.
type Record struct {
	ID           string              `json:"id"`
	Slug         string              `json:"slug,omitempty"`
	Title        string              `json:"title,omitempty"`
	Text         string              `json:"text"`
	SourceRepoID string              `json:"source_repo_id"`
	SourceSlug   string              `json:"source_slug,omitempty"`
	Mode         Mode                `json:"mode"`
	Sensitivity  Sensitivity         `json:"sensitivity"`
	Authority    string              `json:"authority,omitempty"`
	Owner        string              `json:"owner,omitempty"`
	Scope        map[string][]string `json:"scope,omitempty"`
	Priority     int                 `json:"priority,omitempty"`
	ReviewedAt   string              `json:"reviewed_at,omitempty"`
	ExpiresAt    string              `json:"expires_at,omitempty"`
	Supersedes   []string            `json:"supersedes,omitempty"`
	Required     bool                `json:"required,omitempty"`
	Tags         []string            `json:"tags,omitempty"`
	Provenance   Provenance          `json:"provenance"`
}

type LexicalIndex struct {
	Tokens map[string][]string `json:"tokens"`
}

type Manifest struct {
	SchemaVersion    int               `json:"schema_version"`
	BundleRevision   string            `json:"bundle_revision"`
	MinimumReader    string            `json:"minimum_reader"`
	Canonicalization string            `json:"canonicalization"`
	WordListDigest   string            `json:"word_list_digest,omitempty"`
	Sources          []SourceRevision  `json:"sources"`
	Assets           map[string]string `json:"assets"`
	Features         []string          `json:"features,omitempty"`
}

type BundleManifest = Manifest
type SourceLock = SourcesLock

type SourcesLock struct {
	SchemaVersion int              `json:"schema_version"`
	Sources       []SourceRevision `json:"sources"`
}

type Bundle struct {
	Manifest    Manifest          `json:"manifest"`
	SourcesLock SourcesLock       `json:"sources_lock"`
	Records     []Record          `json:"records"`
	Index       LexicalIndex      `json:"index"`
	Assets      map[string][]byte `json:"-"`
	CreatedAt   time.Time         `json:"-"`
}

type CompileOptions struct {
	// MaxChunkBytes defaults to 8192. It is intentionally a byte limit, not a
	// character limit, so output is reproducible across runtimes.
	MaxChunkBytes int
	ReaderVersion string
	CreatedAt     time.Time
}

type SyncResult struct {
	CheckoutDir string `json:"checkout_dir"`
	Commit      string `json:"commit"`
	TreeDigest  string `json:"tree_digest"`
}
