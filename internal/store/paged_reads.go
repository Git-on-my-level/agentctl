package store

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"sort"

	"github.com/Git-on-my-level/agentctl/internal/model"
	"go.etcd.io/bbolt"
)

// Reader shares Cleanup's bounded open/close transport. Listings and inventory
// are advisory views across pages, not a single point-in-time snapshot.
type Reader = Cleanup

func (c Cleanup) ListExecutions(ctx context.Context, active bool) ([]model.Execution, error) {
	r := c.reader(ctx)
	values := []model.Execution{}
	var after []byte
	for {
		var last []byte
		err := r.view(func(tx *bbolt.Tx) error {
			cursor := tx.Bucket(bExecutions).Cursor()
			key, raw := cursor.First()
			if after != nil {
				key, raw = cursor.Seek(after)
				if bytes.Equal(key, after) {
					key, raw = cursor.Next()
				}
			}
			size, count := 0, 0
			for ; key != nil && count < cleanupPageSize && size < cleanupPageBytes; key, raw = cursor.Next() {
				if err := ctx.Err(); err != nil {
					return err
				}
				count++
				size += len(key) + len(raw)
				last = bytes.Clone(key)
				if active {
					var header struct {
						State model.State `json:"state"`
					}
					if err := json.Unmarshal(raw, &header); err != nil {
						return corrupt(err)
					}
					if header.State.Terminal() {
						continue
					}
				}
				var value model.Execution
				if err := decodeExecution(tx, raw, &value); err != nil {
					return corrupt(err)
				}
				if err := value.Validate(); err != nil {
					return corrupt(err)
				}
				values = append(values, value)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if last == nil {
			break
		}
		after = last
	}
	sortExecutions(values)
	return values, nil
}

func sortExecutions(values []model.Execution) {
	sort.Slice(values, func(a, b int) bool {
		if values[a].CreatedAt.Equal(values[b].CreatedAt) {
			return values[a].ID.String() < values[b].ID.String()
		}
		return values[a].CreatedAt.Before(values[b].CreatedAt)
	})
}

// Inventory walks even nested event indexes in bounded pages. No event payload
// is decoded or retained; summaries and child bucket names are copied before
// closing the handle so a writer can acquire between every page.
func (c Cleanup) Inventory(ctx context.Context) (RetentionInventory, error) {
	r := c.reader(ctx)
	result := RetentionInventory{SchemaVersion: RetentionSchemaVersion, GeneratedAt: r.now, Buckets: []BucketUsage{}}
	var names [][]byte
	if err := r.view(func(tx *bbolt.Tx) error {
		return tx.ForEach(func(name []byte, _ *bbolt.Bucket) error { names = append(names, bytes.Clone(name)); return ctx.Err() })
	}); err != nil {
		return result, err
	}
	result.JournalPath = r.path
	info, err := os.Stat(r.path)
	if err != nil {
		return result, err
	}
	result.FileBytes = info.Size()
	var walk func([][]byte, *BucketUsage) error
	walk = func(path [][]byte, usage *BucketUsage) error {
		var after []byte
		for {
			var last []byte
			var children [][]byte
			err := r.view(func(tx *bbolt.Tx) error {
				bucket := tx.Bucket(path[0])
				for _, name := range path[1:] {
					if bucket == nil {
						break
					}
					bucket = bucket.Bucket(name)
				}
				if bucket == nil {
					return nil
				}
				cursor := bucket.Cursor()
				key, raw := cursor.First()
				if after != nil {
					key, raw = cursor.Seek(after)
					if bytes.Equal(key, after) {
						key, raw = cursor.Next()
					}
				}
				size, count := 0, 0
				for ; key != nil && count < cleanupPageSize && size < cleanupPageBytes; key, raw = cursor.Next() {
					if err := ctx.Err(); err != nil {
						return err
					}
					count++
					size += len(key) + len(raw)
					last = bytes.Clone(key)
					usage.KeyBytes += int64(len(key))
					if raw == nil {
						usage.NestedBuckets++
						children = append(children, bytes.Clone(key))
						continue
					}
					usage.Records++
					usage.ValueBytes += int64(len(raw))
					if len(path) != 1 {
						continue
					}
					switch string(path[0]) {
					case string(bExecutions):
						var value model.Execution
						if err := json.Unmarshal(raw, &value); err != nil {
							return corrupt(err)
						}
						if err := value.Validate(); err != nil {
							return corrupt(err)
						}
						result.Executions.Total++
						if value.State.Terminal() {
							result.Executions.Terminal++
						} else {
							result.Executions.NonTerminal++
						}
					case string(bOutcomes):
						var value model.Outcome
						if err := json.Unmarshal(raw, &value); err != nil {
							return corrupt(err)
						}
						if err := value.Validate(); err != nil {
							return corrupt(err)
						}
						result.Executions.Outcomes++
						if value.Content != nil {
							result.Executions.ResultBytes += int64(len(value.Content.Text))
						}
					case string(bEvents):
						result.Executions.Events++
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
			for _, child := range children {
				if err := walk(append(append([][]byte{}, path...), child), usage); err != nil {
					return err
				}
			}
			if last == nil {
				return nil
			}
			after = last
		}
	}
	for _, name := range names {
		usage := BucketUsage{Name: string(name)}
		if err := walk([][]byte{name}, &usage); err != nil {
			return result, err
		}
		result.Buckets = append(result.Buckets, usage)
		result.LogicalBytes += usage.KeyBytes + usage.ValueBytes
	}
	return result, nil
}
