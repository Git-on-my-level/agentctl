package main

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/callback"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/subscription"
)

func (a *app) subscribeCommand(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	if len(args) == 0 {
		return output.NewError(output.CodeUsage, "usage: agentctl subscribe create|list|show|cancel ...", false)
	}
	switch args[0] {
	case "create":
		return a.subscribeCreate(ctx, renderer, c, args[1:])
	case "list":
		if len(args) != 1 {
			return output.NewError(output.CodeUsage, "subscribe list takes no arguments", false)
		}
		journal, problem := a.openRead(c)
		if problem != nil {
			return problem
		}
		defer journal.Close()
		items, err := journal.ListSubscriptions(ctx)
		if err != nil {
			return mapStoreError("list subscriptions", err)
		}
		lines := make([]output.Line, 0, len(items))
		for _, item := range items {
			lines = append(lines, subscriptionLine(item))
		}
		if err := renderer.Success(output.Success{Result: map[string]any{"subscriptions": items, "count": len(items)}, Lines: lines}); err != nil {
			return output.Wrap(output.CodeInternal, "write output", false, err)
		}
		return nil
	case "show":
		if len(args) != 2 {
			return output.NewError(output.CodeUsage, "usage: agentctl subscribe show <subscription-id>", false)
		}
		if _, err := ids.ParseSubscriptionID(args[1]); err != nil {
			return output.Wrap(output.CodeUsage, "invalid subscription ID", false, err)
		}
		journal, problem := a.openRead(c)
		if problem != nil {
			return problem
		}
		defer journal.Close()
		item, err := journal.GetSubscription(ctx, args[1])
		if err != nil {
			return mapStoreError("read subscription", err)
		}
		if err := renderer.Success(output.Success{Result: item, Lines: []output.Line{subscriptionLine(item)}}); err != nil {
			return output.Wrap(output.CodeInternal, "write output", false, err)
		}
		return nil
	case "cancel":
		if len(args) != 2 {
			return output.NewError(output.CodeUsage, "usage: agentctl subscribe cancel <subscription-id>", false)
		}
		if _, err := ids.ParseSubscriptionID(args[1]); err != nil {
			return output.Wrap(output.CodeUsage, "invalid subscription ID", false, err)
		}
		journal, problem := a.openWrite(c)
		if problem != nil {
			return problem
		}
		defer journal.Close()
		record, err := journal.GetSubscriptionRecord(ctx, args[1])
		if err != nil {
			return mapStoreError("read subscription", err)
		}
		value := record.Subscription
		value.State = subscription.StateCancelled
		updated, err := journal.UpdateSubscription(ctx, value, record.Revision)
		if err != nil {
			return mapStoreError("cancel subscription", err)
		}
		if err := renderer.Success(output.Success{Result: updated, Lines: []output.Line{subscriptionLine(updated)}}); err != nil {
			return output.Wrap(output.CodeInternal, "write output", false, err)
		}
		return nil
	default:
		return output.NewError(output.CodeUsage, "subscribe requires create, list, show, or cancel", false)
	}
}

func (a *app) subscribeCreate(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	var executionID, authority, destinationKind, target, credentialRef string
	kinds := []string{string(model.EventTerminal), string(model.EventAttention), string(model.EventArtifact)}
	var commandArgs, allowedHosts []string
	kindsExplicit, allKinds := false, false
	ttl := 24 * time.Hour
	autoExpire := true
	for i := 0; i < len(args); i++ {
		take := func() (string, *output.Error) {
			if i+1 >= len(args) {
				return "", output.NewError(output.CodeUsage, args[i]+" requires a value", false)
			}
			i++
			return args[i], nil
		}
		switch args[i] {
		case "--execution":
			value, problem := take()
			if problem != nil {
				return problem
			}
			executionID = value
		case "--authority":
			value, problem := take()
			if problem != nil {
				return problem
			}
			authority = value
		case "--kind":
			value, problem := take()
			if problem != nil {
				return problem
			}
			values := splitValues(value)
			if len(values) == 1 && strings.EqualFold(values[0], "all") {
				if kindsExplicit && !allKinds {
					return output.NewError(output.CodeUsage, "--kind all cannot be combined with named event kinds", false)
				}
				kinds = nil
				kindsExplicit, allKinds = true, true
			} else if len(values) == 0 {
				return output.NewError(output.CodeUsage, "--kind requires event names or all", false)
			} else {
				if allKinds {
					return output.NewError(output.CodeUsage, "--kind all cannot be combined with named event kinds", false)
				}
				if !kindsExplicit {
					kinds = nil
					kindsExplicit = true
				}
				kinds = append(kinds, values...)
			}
		case "--destination":
			value, problem := take()
			if problem != nil {
				return problem
			}
			destinationKind = value
		case "--target":
			value, problem := take()
			if problem != nil {
				return problem
			}
			target = value
		case "--arg":
			value, problem := take()
			if problem != nil {
				return problem
			}
			commandArgs = append(commandArgs, value)
		case "--allow-host":
			value, problem := take()
			if problem != nil {
				return problem
			}
			allowedHosts = append(allowedHosts, value)
		case "--credential-ref":
			value, problem := take()
			if problem != nil {
				return problem
			}
			credentialRef = value
		case "--ttl":
			value, problem := take()
			if problem != nil {
				return problem
			}
			parsed, err := time.ParseDuration(value)
			if err != nil || parsed <= 0 {
				return output.NewError(output.CodeUsage, "ttl must be a positive Go duration", false)
			}
			ttl = parsed
		case "--keep-after-terminal":
			autoExpire = false
		default:
			return output.NewError(output.CodeUsage, "unknown subscribe create flag", false).WithDetail("flag", args[i])
		}
	}
	if executionID == "" && authority == "" {
		return output.NewError(output.CodeUsage, "subscribe create requires --execution or --authority", false)
	}
	if executionID != "" {
		if _, err := ids.ParseExecutionID(executionID); err != nil {
			return output.Wrap(output.CodeUsage, "invalid execution ID", false, err)
		}
	}
	destination := callback.Destination{Kind: callback.DestinationKind(destinationKind), Path: target, Args: commandArgs, CredentialRef: credentialRef, AllowedHosts: allowedHosts}
	switch destination.Kind {
	case callback.DestinationFile, callback.DestinationUnix, callback.DestinationCommand:
		if !filepath.IsAbs(destination.Path) {
			return output.NewError(output.CodeUsage, "persistent local destination paths must be absolute", false)
		}
	case callback.DestinationWebhook:
	default:
		return output.NewError(output.CodeUsage, "persistent destination must be file, unix, webhook, or command", false)
	}
	if err := destination.Validate(); err != nil {
		return output.Wrap(output.CodeUsage, "invalid callback destination", false, err)
	}
	filter := subscription.EventFilter{Authority: authority, Kinds: kinds}
	if executionID != "" {
		filter.ExecutionIDs = []string{executionID}
	}
	now := a.now().UTC()
	value := subscription.Subscription{Filter: filter, Destination: destination, ExpiresAt: now.Add(ttl), State: subscription.StateActive, AutoExpireOnTerminal: autoExpire}
	journal, problem := a.openWrite(c)
	if problem != nil {
		return problem
	}
	defer journal.Close()
	created, reused, err := journal.PutSubscription(ctx, value)
	if err != nil {
		return mapStoreError("create subscription", err)
	}
	if err := renderer.Success(output.Success{Result: map[string]any{"subscription": created, "reused": reused}, Lines: []output.Line{subscriptionLine(created)}}); err != nil {
		return output.Wrap(output.CodeInternal, "write output", false, err)
	}
	return nil
}

func subscriptionLine(value subscription.Subscription) output.Line {
	return output.Line{Lead: value.ID, Fields: []output.Field{{Name: "state", Value: value.State}, {Name: "destination", Value: value.Destination.Kind}, {Name: "expires", Value: value.ExpiresAt.UTC().Format(time.RFC3339)}, {Name: "executions", Value: len(value.Filter.ExecutionIDs)}, {Name: "kinds", Value: value.Filter.Kinds}}}
}

func splitValues(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}
