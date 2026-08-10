package ids

import (
	"encoding/json"
	"fmt"
)

type typedID string

func parseTyped(typ Type, value string) (typedID, error) {
	if _, err := ParseAs(typ, value); err != nil {
		return "", err
	}
	return typedID(value), nil
}
func marshalTyped(value typedID, typ Type) ([]byte, error) {
	if _, err := ParseAs(typ, string(value)); err != nil {
		return nil, err
	}
	return json.Marshal(string(value))
}
func unmarshalTyped(data []byte, typ Type) (typedID, error) {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return "", err
	}
	return parseTyped(typ, value)
}

// Concrete ID types prevent mixing object identifiers at package boundaries.
type ExecutionID typedID
type EventID typedID
type HostID typedID
type CursorID typedID
type SubscriptionID typedID
type DeliveryID typedID
type ArtifactID typedID
type ContextID typedID
type RouteID typedID
type RepositoryID typedID
type KnowledgeID typedID
type SourceID typedID
type ProjectID typedID
type IssueID typedID
type RunID typedID

func typedString[T ~string](v T) string { return string(v) }
func typedZero[T ~string](v T) bool     { return v == "" }

func ParseExecutionID(v string) (ExecutionID, error) {
	x, e := parseTyped(TypeExecution, v)
	return ExecutionID(x), e
}
func ParseEventID(v string) (EventID, error) { x, e := parseTyped(TypeEvent, v); return EventID(x), e }
func ParseHostID(v string) (HostID, error)   { x, e := parseTyped(TypeHost, v); return HostID(x), e }
func ParseCursorID(v string) (CursorID, error) {
	x, e := parseTyped(TypeCursor, v)
	return CursorID(x), e
}
func ParseSubscriptionID(v string) (SubscriptionID, error) {
	x, e := parseTyped(TypeSubscription, v)
	return SubscriptionID(x), e
}
func ParseDeliveryID(v string) (DeliveryID, error) {
	x, e := parseTyped(TypeDelivery, v)
	return DeliveryID(x), e
}
func ParseArtifactID(v string) (ArtifactID, error) {
	x, e := parseTyped(TypeArtifact, v)
	return ArtifactID(x), e
}
func ParseContextID(v string) (ContextID, error) {
	x, e := parseTyped(TypeContext, v)
	return ContextID(x), e
}
func ParseRouteID(v string) (RouteID, error) { x, e := parseTyped(TypeRoute, v); return RouteID(x), e }
func ParseRepositoryID(v string) (RepositoryID, error) {
	x, e := parseTyped(TypeRepository, v)
	return RepositoryID(x), e
}
func ParseKnowledgeID(v string) (KnowledgeID, error) {
	x, e := parseTyped(TypeKnowledge, v)
	return KnowledgeID(x), e
}
func ParseSourceID(v string) (SourceID, error) {
	x, e := parseTyped(TypeSource, v)
	return SourceID(x), e
}
func ParseProjectID(v string) (ProjectID, error) {
	x, e := parseTyped(TypeProject, v)
	return ProjectID(x), e
}
func ParseIssueID(v string) (IssueID, error) { x, e := parseTyped(TypeIssue, v); return IssueID(x), e }
func ParseRunID(v string) (RunID, error)     { x, e := parseTyped(TypeRun, v); return RunID(x), e }

func newTyped[T ~string](typ Type, generator Generator) (T, error) {
	id, e := generator.New(typ)
	if e != nil {
		return "", e
	}
	return T(id.String()), nil
}
func NewExecutionID(g Generator) (ExecutionID, error) { return newTyped[ExecutionID](TypeExecution, g) }
func NewEventID(g Generator) (EventID, error)         { return newTyped[EventID](TypeEvent, g) }
func NewHostID(g Generator) (HostID, error)           { return newTyped[HostID](TypeHost, g) }
func NewCursorID(g Generator) (CursorID, error)       { return newTyped[CursorID](TypeCursor, g) }
func NewSubscriptionID(g Generator) (SubscriptionID, error) {
	return newTyped[SubscriptionID](TypeSubscription, g)
}
func NewDeliveryID(g Generator) (DeliveryID, error) { return newTyped[DeliveryID](TypeDelivery, g) }
func NewArtifactID(g Generator) (ArtifactID, error) { return newTyped[ArtifactID](TypeArtifact, g) }
func NewContextID(g Generator) (ContextID, error)   { return newTyped[ContextID](TypeContext, g) }
func NewRouteID(g Generator) (RouteID, error)       { return newTyped[RouteID](TypeRoute, g) }
func NewRepositoryID(g Generator) (RepositoryID, error) {
	return newTyped[RepositoryID](TypeRepository, g)
}
func NewKnowledgeID(g Generator) (KnowledgeID, error) { return newTyped[KnowledgeID](TypeKnowledge, g) }
func NewSourceID(g Generator) (SourceID, error)       { return newTyped[SourceID](TypeSource, g) }
func NewProjectID(g Generator) (ProjectID, error)     { return newTyped[ProjectID](TypeProject, g) }
func NewIssueID(g Generator) (IssueID, error)         { return newTyped[IssueID](TypeIssue, g) }
func NewRunID(g Generator) (RunID, error)             { return newTyped[RunID](TypeRun, g) }

func (v ExecutionID) String() string    { return typedString(v) }
func (v ExecutionID) IsZero() bool      { return typedZero(v) }
func (v EventID) String() string        { return typedString(v) }
func (v EventID) IsZero() bool          { return typedZero(v) }
func (v HostID) String() string         { return typedString(v) }
func (v HostID) IsZero() bool           { return typedZero(v) }
func (v CursorID) String() string       { return typedString(v) }
func (v CursorID) IsZero() bool         { return typedZero(v) }
func (v SubscriptionID) String() string { return typedString(v) }
func (v SubscriptionID) IsZero() bool   { return typedZero(v) }
func (v DeliveryID) String() string     { return typedString(v) }
func (v DeliveryID) IsZero() bool       { return typedZero(v) }
func (v ArtifactID) String() string     { return typedString(v) }
func (v ArtifactID) IsZero() bool       { return typedZero(v) }
func (v ContextID) String() string      { return typedString(v) }
func (v ContextID) IsZero() bool        { return typedZero(v) }
func (v RouteID) String() string        { return typedString(v) }
func (v RouteID) IsZero() bool          { return typedZero(v) }
func (v RepositoryID) String() string   { return typedString(v) }
func (v RepositoryID) IsZero() bool     { return typedZero(v) }
func (v KnowledgeID) String() string    { return typedString(v) }
func (v KnowledgeID) IsZero() bool      { return typedZero(v) }
func (v SourceID) String() string       { return typedString(v) }
func (v SourceID) IsZero() bool         { return typedZero(v) }
func (v ProjectID) String() string      { return typedString(v) }
func (v ProjectID) IsZero() bool        { return typedZero(v) }
func (v IssueID) String() string        { return typedString(v) }
func (v IssueID) IsZero() bool          { return typedZero(v) }
func (v RunID) String() string          { return typedString(v) }
func (v RunID) IsZero() bool            { return typedZero(v) }

func marshal(v string, typ Type) ([]byte, error)   { return marshalTyped(typedID(v), typ) }
func (v ExecutionID) MarshalJSON() ([]byte, error) { return marshal(string(v), TypeExecution) }
func (v *ExecutionID) UnmarshalJSON(b []byte) error {
	x, e := unmarshalTyped(b, TypeExecution)
	if e == nil {
		*v = ExecutionID(x)
	}
	return e
}
func (v EventID) MarshalJSON() ([]byte, error) { return marshal(string(v), TypeEvent) }
func (v *EventID) UnmarshalJSON(b []byte) error {
	x, e := unmarshalTyped(b, TypeEvent)
	if e == nil {
		*v = EventID(x)
	}
	return e
}
func (v HostID) MarshalJSON() ([]byte, error) { return marshal(string(v), TypeHost) }
func (v *HostID) UnmarshalJSON(b []byte) error {
	x, e := unmarshalTyped(b, TypeHost)
	if e == nil {
		*v = HostID(x)
	}
	return e
}

func (v CursorID) MarshalJSON() ([]byte, error) { return marshal(string(v), TypeCursor) }
func (v *CursorID) UnmarshalJSON(b []byte) error {
	x, e := unmarshalTyped(b, TypeCursor)
	if e == nil {
		*v = CursorID(x)
	}
	return e
}
func (v SubscriptionID) MarshalJSON() ([]byte, error) { return marshal(string(v), TypeSubscription) }
func (v *SubscriptionID) UnmarshalJSON(b []byte) error {
	x, e := unmarshalTyped(b, TypeSubscription)
	if e == nil {
		*v = SubscriptionID(x)
	}
	return e
}
func (v DeliveryID) MarshalJSON() ([]byte, error) { return marshal(string(v), TypeDelivery) }
func (v *DeliveryID) UnmarshalJSON(b []byte) error {
	x, e := unmarshalTyped(b, TypeDelivery)
	if e == nil {
		*v = DeliveryID(x)
	}
	return e
}
func (v ArtifactID) MarshalJSON() ([]byte, error) { return marshal(string(v), TypeArtifact) }
func (v *ArtifactID) UnmarshalJSON(b []byte) error {
	x, e := unmarshalTyped(b, TypeArtifact)
	if e == nil {
		*v = ArtifactID(x)
	}
	return e
}
func (v ContextID) MarshalJSON() ([]byte, error) { return marshal(string(v), TypeContext) }
func (v *ContextID) UnmarshalJSON(b []byte) error {
	x, e := unmarshalTyped(b, TypeContext)
	if e == nil {
		*v = ContextID(x)
	}
	return e
}
func (v RouteID) MarshalJSON() ([]byte, error) { return marshal(string(v), TypeRoute) }
func (v *RouteID) UnmarshalJSON(b []byte) error {
	x, e := unmarshalTyped(b, TypeRoute)
	if e == nil {
		*v = RouteID(x)
	}
	return e
}
func (v RepositoryID) MarshalJSON() ([]byte, error) { return marshal(string(v), TypeRepository) }
func (v *RepositoryID) UnmarshalJSON(b []byte) error {
	x, e := unmarshalTyped(b, TypeRepository)
	if e == nil {
		*v = RepositoryID(x)
	}
	return e
}
func (v KnowledgeID) MarshalJSON() ([]byte, error) { return marshal(string(v), TypeKnowledge) }
func (v *KnowledgeID) UnmarshalJSON(b []byte) error {
	x, e := unmarshalTyped(b, TypeKnowledge)
	if e == nil {
		*v = KnowledgeID(x)
	}
	return e
}
func (v SourceID) MarshalJSON() ([]byte, error) { return marshal(string(v), TypeSource) }
func (v *SourceID) UnmarshalJSON(b []byte) error {
	x, e := unmarshalTyped(b, TypeSource)
	if e == nil {
		*v = SourceID(x)
	}
	return e
}
func (v ProjectID) MarshalJSON() ([]byte, error) { return marshal(string(v), TypeProject) }
func (v *ProjectID) UnmarshalJSON(b []byte) error {
	x, e := unmarshalTyped(b, TypeProject)
	if e == nil {
		*v = ProjectID(x)
	}
	return e
}
func (v IssueID) MarshalJSON() ([]byte, error) { return marshal(string(v), TypeIssue) }
func (v *IssueID) UnmarshalJSON(b []byte) error {
	x, e := unmarshalTyped(b, TypeIssue)
	if e == nil {
		*v = IssueID(x)
	}
	return e
}
func (v RunID) MarshalJSON() ([]byte, error) { return marshal(string(v), TypeRun) }
func (v *RunID) UnmarshalJSON(b []byte) error {
	x, e := unmarshalTyped(b, TypeRun)
	if e == nil {
		*v = RunID(x)
	}
	return e
}

func ValidateTyped(typ Type, value fmt.Stringer) error {
	_, err := ParseAs(typ, value.String())
	return err
}
