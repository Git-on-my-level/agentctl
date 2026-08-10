package callback

// Producer is the adapter-facing semantic event identity helper. Adapters
// construct one producer per adapter/version and call Produce for every
// normalized semantic projection. Observation time, local IDs, and delivery
// attempts do not belong in Projection.
type Producer struct {
	Adapter string
	Version uint32
}

// SemanticEvent carries the exact canonical projection alongside its full
// dedupe key so a journal can detect a hash collision instead of overwriting
// an unequal projection.
type SemanticEvent struct {
	DedupeKey     string
	DedupeVersion uint32
	Projection    []byte
}

func NewProducer(adapter string, version uint32) (Producer, error) {
	if _, _, err := SemanticDedupeKey(adapter, version, nil); err != nil {
		return Producer{}, err
	}
	return Producer{Adapter: adapter, Version: version}, nil
}

func (p Producer) Produce(projection any) (SemanticEvent, error) {
	key, canonical, err := SemanticDedupeKey(p.Adapter, p.Version, projection)
	if err != nil {
		return SemanticEvent{}, err
	}
	return SemanticEvent{DedupeKey: key, DedupeVersion: p.Version, Projection: canonical}, nil
}
