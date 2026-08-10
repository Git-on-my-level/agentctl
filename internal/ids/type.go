package ids

import "sort"

// Type is a registered agentctl word-ID prefix. The registry is closed for
// encoding version 1; adding a value is a wire-contract change.
type Type string

const (
	TypeExecution    Type = "exec"
	TypeEvent        Type = "event"
	TypeSubscription Type = "sub"
	TypeCursor       Type = "cursor"
	TypeDelivery     Type = "delivery"
	TypeArtifact     Type = "artifact"
	TypeContext      Type = "context"
	TypeRoute        Type = "route"
	TypeHost         Type = "host"
	TypeRepository   Type = "repo"
	TypeKnowledge    Type = "knowledge"
	TypeSource       Type = "source"
	TypeProject      Type = "project"
	TypeIssue        Type = "issue"
	TypeRun          Type = "run"
)

var registeredTypes = map[Type]struct{}{
	TypeExecution: {}, TypeEvent: {}, TypeSubscription: {}, TypeCursor: {},
	TypeDelivery: {}, TypeArtifact: {}, TypeContext: {}, TypeRoute: {},
	TypeHost: {}, TypeRepository: {}, TypeKnowledge: {}, TypeSource: {},
	TypeProject: {}, TypeIssue: {}, TypeRun: {},
}

func (t Type) Valid() bool { _, ok := registeredTypes[t]; return ok }

func Types() []Type {
	types := make([]Type, 0, len(registeredTypes))
	for typ := range registeredTypes {
		types = append(types, typ)
	}
	sort.Slice(types, func(i, j int) bool { return types[i] < types[j] })
	return types
}
