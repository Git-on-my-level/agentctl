package knowledge

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// LoadBundle verifies every bundle component before reconstructing records
// from the verified asset JSON. It never reads an unlisted file and never
// follows symlink path components.
func LoadBundle(dir string) (Bundle, error) {
	manifest, err := VerifyBundle(dir)
	if err != nil {
		return Bundle{}, err
	}
	var lock SourcesLock
	if err := readJSONFile(filepath.Join(dir, "sources.lock.json"), &lock); err != nil {
		return Bundle{}, err
	}
	var index LexicalIndex
	if err := readJSONFile(filepath.Join(dir, "index.json"), &index); err != nil {
		return Bundle{}, err
	}
	if err := validateSources(lock, manifest); err != nil {
		return Bundle{}, err
	}

	names := make([]string, 0, len(manifest.Assets))
	for name := range manifest.Assets {
		names = append(names, name)
	}
	sort.Strings(names)
	records := make([]Record, 0, len(names))
	assets := make(map[string][]byte, len(names))
	seenIDs := make(map[string]bool, len(names))
	for _, name := range names {
		data, err := secureReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			return Bundle{}, fmt.Errorf("asset %s: %w", name, err)
		}
		var record Record
		if err := json.Unmarshal(data, &record); err != nil {
			return Bundle{}, fmt.Errorf("asset %s: invalid record JSON: %w", name, err)
		}
		if err := validateLoadedRecord(record, lock); err != nil {
			return Bundle{}, fmt.Errorf("asset %s: %w", name, err)
		}
		if seenIDs[record.ID] {
			return Bundle{}, fmt.Errorf("asset %s: duplicate record id %s", name, record.ID)
		}
		seenIDs[record.ID] = true
		records = append(records, record)
		assets[name] = data
	}
	if err := validateLoadedIndex(index, seenIDs); err != nil {
		return Bundle{}, err
	}
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].ID != records[j].ID {
			return records[i].ID < records[j].ID
		}
		return records[i].Provenance.Path < records[j].Provenance.Path
	})
	return Bundle{Manifest: manifest, SourcesLock: lock, Records: records, Index: index, Assets: assets}, nil
}

func validateSources(lock SourcesLock, manifest Manifest) error {
	if lock.SchemaVersion != 1 {
		return errors.New("unsupported sources lock schema")
	}
	if len(lock.Sources) != len(manifest.Sources) {
		return errors.New("sources lock contains a different source count")
	}
	seen := make(map[string]bool, len(lock.Sources))
	for _, source := range lock.Sources {
		if source.ID == "" || seen[source.ID] {
			return fmt.Errorf("invalid or duplicate source id %q", source.ID)
		}
		seen[source.ID] = true
		if source.Commit == "" || source.TreeDigest == "" || source.ContentDigest == "" {
			return fmt.Errorf("source %s has incomplete revision provenance", source.ID)
		}
	}
	return nil
}

func validateLoadedRecord(record Record, lock SourcesLock) error {
	if strings.TrimSpace(record.ID) == "" {
		return errors.New("record id is required")
	}
	if record.SourceRepoID == "" {
		return errors.New("record source_repo_id is required")
	}
	foundSource := false
	for _, source := range lock.Sources {
		if source.ID == record.SourceRepoID {
			foundSource = true
			break
		}
	}
	if !foundSource {
		return fmt.Errorf("record references unknown source %s", record.SourceRepoID)
	}
	provenance := record.Provenance
	if provenance.SourceRepoID != record.SourceRepoID {
		return errors.New("provenance source_repo_id does not match record")
	}
	if provenance.SourceCommit == "" {
		return errors.New("provenance source_commit is required")
	}
	if provenance.Path == "" || filepath.IsAbs(provenance.Path) || strings.Contains(provenance.Path, "\\") {
		return errors.New("provenance path must be a relative slash path")
	}
	cleanPath := filepath.ToSlash(filepath.Clean(provenance.Path))
	if cleanPath == "." || cleanPath == ".." || strings.HasPrefix(cleanPath, "../") {
		return errors.New("provenance path escapes source")
	}
	if provenance.StartLine < 1 || provenance.EndLine < provenance.StartLine {
		return errors.New("provenance line range is invalid")
	}
	if provenance.ContentDigest == "" || provenance.ContentDigest != digestBytes([]byte(record.Text)) {
		return errors.New("provenance content_digest does not match record text")
	}
	if record.Mode != ModeStructured && record.Mode != ModeLoose && record.Mode != ModeHybrid {
		return errors.New("record mode is invalid")
	}
	if sensitivityRank(record.Sensitivity) < 0 {
		return errors.New("record sensitivity is invalid")
	}
	return nil
}

func validateLoadedIndex(index LexicalIndex, recordIDs map[string]bool) error {
	for token, ids := range index.Tokens {
		if strings.TrimSpace(token) == "" || token != strings.ToLower(token) {
			return errors.New("lexical index contains an invalid token")
		}
		for _, r := range token {
			if !unicode.IsLetter(r) && !unicode.IsNumber(r) {
				return fmt.Errorf("lexical index token %q contains punctuation", token)
			}
		}
		if len(ids) == 0 {
			return fmt.Errorf("lexical index token %q has no records", token)
		}
		previous := ""
		for _, id := range ids {
			if !recordIDs[id] {
				return fmt.Errorf("lexical index token %q references unknown record %s", token, id)
			}
			if previous != "" && id <= previous {
				return fmt.Errorf("lexical index token %q record IDs are not strictly sorted", token)
			}
			previous = id
		}
	}
	return nil
}
