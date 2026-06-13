package server

import (
	"context"

	"github.com/velocitykode/velocity/str"
)

// MaxCompletionValues is the maximum number of values a completion/complete
// result may carry, per the MCP spec. CompleteValues truncates to this bound
// and reports the overflow via Completion.HasMore.
const MaxCompletionValues = 100

// CompletionRequest describes an in-progress argument completion: the argument
// being completed, the partial value typed so far, and any sibling arguments
// the client has already resolved (the completion context). It is passed to a
// Completable primitive's Complete method.
type CompletionRequest struct {
	// Argument is the name of the argument being completed (a prompt argument
	// name or a resource URI-template variable name).
	Argument string
	// Value is the partial value typed so far; completions should match it.
	Value string
	// Context holds sibling argument values the client has already resolved,
	// keyed by argument name. It may be nil.
	Context map[string]string
}

// Completion is the result of a completion request: the candidate values for
// the argument, the total number of matches available, and whether more values
// exist beyond those returned.
type Completion struct {
	// Values are the candidate completions, capped at MaxCompletionValues.
	Values []string
	// Total is the number of matches available; when zero it defaults to the
	// number of Values on the wire.
	Total int
	// HasMore reports whether matches were truncated.
	HasMore bool
}

// Completable is implemented by prompts and resources that supply value
// completions for their arguments (prompt arguments or resource URI-template
// variables) in completion/complete. A resolvable primitive that does not
// implement it yields an empty completion.
type Completable interface {
	Complete(ctx context.Context, req CompletionRequest) (Completion, error)
}

// CompleteValues builds a Completion from candidate values, keeping those that
// start with the request value (case-insensitive) and capping the result at
// MaxCompletionValues. Total reflects the full match count and HasMore is set
// when the matches were truncated. It is the common helper a Completable uses
// to answer from a static or computed candidate list. An empty value matches
// every candidate.
func CompleteValues(value string, candidates []string) Completion {
	prefix := str.Lower(value)
	matched := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if prefix == "" || str.StartsWith(str.Lower(c), prefix) {
			matched = append(matched, c)
		}
	}
	total := len(matched)
	hasMore := false
	if total > MaxCompletionValues {
		matched = matched[:MaxCompletionValues]
		hasMore = true
	}
	return Completion{Values: matched, Total: total, HasMore: hasMore}
}
