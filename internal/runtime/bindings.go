package runtime

import (
	"fmt"
	"strconv"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
)

const (
	bindingExecutable       = "runtime_executable"
	bindingMulticaProfile   = "multica_profile"
	bindingMulticaEndpoint  = "multica_endpoint"
	bindingMulticaWorkspace = "multica_workspace"
	bindingMulticaIssue     = "multica_issue"
	bindingMulticaRun       = "multica_run"
)

func (e *Engine) configBindings(authority model.Authority, spec AdapterSpec) ([]model.SourceBinding, error) {
	return e.configBindingsPreserving(authority, spec, nil)
}

func (e *Engine) configBindingsPreserving(authority model.Authority, spec AdapterSpec, existing []model.SourceBinding) ([]model.SourceBinding, error) {
	bindings := []model.SourceBinding{}
	executable := spec.Executable
	if spec.Multica != nil && executable == "" {
		executable = spec.Multica.Binary
		if executable == "" {
			executable = "multica"
		}
	}
	if executable != "" {
		binding, err := e.bindingOrNew(authority, bindingExecutable, executable, ids.TypeSource, existing)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, binding)
	}
	if spec.Multica == nil {
		return bindings, nil
	}
	for _, item := range []struct {
		kind  string
		value string
		type_ ids.Type
	}{
		{bindingMulticaProfile, spec.Multica.Profile, ids.TypeSource},
		{bindingMulticaEndpoint, spec.Multica.Endpoint, ids.TypeSource},
		{bindingMulticaWorkspace, spec.Multica.Workspace, ids.TypeProject},
		{bindingMulticaIssue, spec.Multica.Issue, ids.TypeIssue},
		{bindingMulticaRun, spec.Multica.Run, ids.TypeRun},
	} {
		if item.value == "" {
			continue
		}
		binding, err := e.bindingOrNew(authority, item.kind, item.value, item.type_, existing)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, binding)
	}
	return bindings, nil
}

func (e *Engine) sessionBindings(execution model.Execution, spec AdapterSpec, session adapter.Session) ([]model.SourceBinding, error) {
	bindings, err := e.configBindingsPreserving(execution.Authority, spec, execution.SourceBindings)
	if err != nil {
		return nil, err
	}
	ref := session.Ref
	if ref.Kind == "" {
		return nil, &Error{Code: CodeUnsafeObservation, Operation: "normalize_session", Adapter: session.Ref.Adapter, Message: "adapter session omitted source kind"}
	}
	fingerprint := session.Binding.Fingerprint
	if fingerprint == "" {
		fingerprint = ref.Binding().Fingerprint
	}
	value := ref.OpaqueID
	// The configured Multica run binding already represents this exact source.
	if execution.Authority == model.AuthorityMultica && ref.Kind == bindingMulticaRun && value == spec.Multica.Run {
		return bindings, nil
	}
	for _, existing := range execution.SourceBindings {
		if existing.Kind == ref.Kind && existing.Fingerprint == fingerprint {
			return append(bindings, existing), nil
		}
	}
	alias, err := e.generator.New(ids.TypeSource)
	if err != nil {
		return nil, fmt.Errorf("allocate source alias: %w", err)
	}
	var opaque *string
	if value != "" {
		copy := value
		opaque = &copy
	}
	bindings = append(bindings, model.SourceBinding{Kind: ref.Kind, AliasID: alias, Fingerprint: fingerprint, OpaqueID: opaque})
	return bindings, nil
}

func (e *Engine) newBinding(authority model.Authority, kind, value string, typ ids.Type) (model.SourceBinding, error) {
	alias, err := e.generator.New(typ)
	if err != nil {
		return model.SourceBinding{}, fmt.Errorf("allocate %s alias: %w", kind, err)
	}
	copy := value
	return model.SourceBinding{Kind: kind, AliasID: alias, Fingerprint: adapter.Fingerprint(string(authority), kind, value), OpaqueID: &copy}, nil
}

func (e *Engine) bindingOrNew(authority model.Authority, kind, value string, typ ids.Type, existing []model.SourceBinding) (model.SourceBinding, error) {
	fingerprint := adapter.Fingerprint(string(authority), kind, value)
	for _, binding := range existing {
		if binding.Kind == kind && binding.Fingerprint == fingerprint {
			return binding, nil
		}
	}
	return e.newBinding(authority, kind, value, typ)
}

func decodeBindings(execution model.Execution) (AdapterSpec, adapter.SourceRef, error) {
	spec := AdapterSpec{Name: execution.Adapter}
	values := map[string]string{}
	var source *model.SourceBinding
	for index := range execution.SourceBindings {
		binding := &execution.SourceBindings[index]
		value := ""
		if binding.OpaqueID != nil {
			value = *binding.OpaqueID
		}
		values[binding.Kind] = value
		if !configurationBinding(binding.Kind) && source == nil {
			source = binding
		}
	}
	spec.Executable = values[bindingExecutable]
	if execution.Authority == model.AuthorityMultica {
		spec.Name = "multica"
		spec.Multica = &adapter.MulticaConfig{
			Binary: spec.Executable, Profile: values[bindingMulticaProfile], Endpoint: values[bindingMulticaEndpoint],
			Workspace: values[bindingMulticaWorkspace], Issue: values[bindingMulticaIssue], Run: values[bindingMulticaRun],
		}
		if err := spec.Validate(); err != nil {
			return AdapterSpec{}, adapter.SourceRef{}, &Error{Code: CodeInvalidConfiguration, Operation: "decode_bindings", Adapter: execution.Adapter, Message: "stored Multica authority bindings are incomplete", Cause: err}
		}
		kind, opaqueID := "multica_event", ""
		if spec.Multica.Issue != "" {
			kind, opaqueID = bindingMulticaIssue, spec.Multica.Issue
		}
		if spec.Multica.Run != "" {
			kind, opaqueID = bindingMulticaRun, spec.Multica.Run
		}
		ref := adapter.SourceRef{Adapter: execution.Adapter, Kind: kind, OpaqueID: opaqueID, Profile: spec.Multica.Profile, Endpoint: spec.Multica.Endpoint, Workspace: spec.Multica.Workspace, Issue: spec.Multica.Issue, Run: spec.Multica.Run}
		for _, binding := range execution.SourceBindings {
			if binding.Kind == kind {
				ref.Fingerprint = binding.Fingerprint
				break
			}
		}
		return spec, ref, nil
	}
	if source == nil {
		return AdapterSpec{}, adapter.SourceRef{}, &Error{Code: CodeInvalidConfiguration, Operation: "decode_bindings", Adapter: execution.Adapter, Message: "stored execution has no native source binding"}
	}
	ref := adapter.SourceRef{Adapter: execution.Adapter, Kind: source.Kind, Fingerprint: source.Fingerprint}
	if source.OpaqueID != nil {
		ref.OpaqueID = *source.OpaqueID
		ref.PID, _ = strconv.Atoi(ref.OpaqueID)
	}
	return spec, ref, nil
}

func configurationBinding(kind string) bool {
	switch kind {
	case bindingExecutable, bindingMulticaProfile, bindingMulticaEndpoint, bindingMulticaWorkspace, bindingMulticaIssue, bindingMulticaRun:
		return true
	default:
		return false
	}
}
