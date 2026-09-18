package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/velocitykode/velocity/contract"
	"github.com/velocitykode/velocity/str"
	"github.com/velocitykode/velocity/validation"

	"github.com/velocitykode/velocity-mcp/content"
	"github.com/velocitykode/velocity-mcp/event"
	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/schema"
)

// Tool catalog defaults and floors. A server with hundreds of tools blows past
// what a client can hold in context if every one is listed, so the catalog
// exposes them behind two meta tools instead: one that searches the catalog and
// one that runs a batch of catalog tools. The floors keep a misconfigured limit
// from disabling the feature outright.
const (
	// catalogSearchToolName is the fixed name of the catalog search tool. Both
	// meta-tool names are part of the catalog's client-facing contract (agents
	// are told to call them by name in the tool descriptions), so they are not
	// derived from a Go type like an ordinary primitive name.
	catalogSearchToolName = "search_tools"
	// catalogExecuteToolName is the fixed name of the catalog batch-execute tool.
	catalogExecuteToolName = "execute_tools"

	// defaultCatalogMaxToolCalls is the default cap on calls per execute batch.
	defaultCatalogMaxToolCalls = 25
	// defaultCatalogMaxOutputBytes is the default cap on the encoded size of a
	// catalog tool's reply.
	defaultCatalogMaxOutputBytes = 65536
	// minCatalogMaxToolCalls is the floor for the per-batch call cap.
	minCatalogMaxToolCalls = 1
	// minCatalogMaxOutputBytes is the floor for the output size cap. A limit
	// the catalog cannot report within is worse than no limit at all, so the
	// floor leaves room for the smallest report the catalog can be reduced to:
	// the diagnostics, the completeness flag, the three call counters and the
	// index of the omitted call, measured as the client receives the reply.
	minCatalogMaxOutputBytes = 512
	// defaultCatalogSearchLimit is the number of results a search returns when
	// the client does not ask for a specific limit.
	defaultCatalogSearchLimit = 10
	// maxCatalogQueryChars is the accepted length of a search query, in
	// characters: the unit the advertised "maxLength" keyword is defined in.
	maxCatalogQueryChars = 4096
	// maxCatalogSearchLimit is the largest result count a client may request.
	maxCatalogSearchLimit = 50
	// maxCatalogCallNameChars is the accepted length of a call's tool name, in
	// characters.
	maxCatalogCallNameChars = 255
	// catalogKindOutputLimit names the failure of a payload that would exceed
	// the catalog's output size limit.
	catalogKindOutputLimit = "OutputLimitExceeded"
	// catalogKindEncodeFailure names the failure of a result the catalog could
	// not render as JSON.
	catalogKindEncodeFailure = "OutputEncodingFailed"
	// catalogEncodeFailureMessage is the client-facing wording of an encoding
	// failure. It names no internal detail.
	catalogEncodeFailureMessage = "The tool catalog could not encode the tool output."
	// catalogTruncationMarker ends a diagnostic string that had to be shortened
	// to hold a reply to the size limit it reports, so a client can tell a
	// clipped value from a complete one.
	catalogTruncationMarker = "..."
	// catalogMetaKey is the _meta member of an execute batch result under which
	// the metadata of the inner results travels. A tool's metadata is addressed
	// to the host rather than to the model, so it is carried in the metadata
	// channel of the batch result instead of being written into the text the
	// model reads.
	catalogMetaKey = "com.velocitykode.mcp/toolCatalog"
)

// toolCatalog holds the tools reachable through the two catalog meta tools plus
// the batch and output limits applied to them.
//
// A catalog is built and populated while New applies its options (a single
// goroutine) and is only read afterwards, so it needs no locking: the meta
// tools never mutate it while serving a request.
type toolCatalog struct {
	// server is the owning server, used to reach the event dispatcher and the
	// logger when a catalog tool is invoked. It is set when the catalog is
	// created; the dispatch helpers still tolerate a nil server so a catalog
	// built by hand can never panic.
	server *Server
	// tools are the catalog entries, in registration order. The order is the
	// tie-breaker for equally scored search results.
	tools []Tool
	// entries are the prepared form of tools, in the same order. They are built
	// once, after every option has been applied, so serving a request neither
	// rebuilds a schema nor re-encodes one.
	entries []catalogEntry
	// dupErr, when set, is the client-facing message reported by both meta
	// tools when two catalog entries share a name. A duplicate makes
	// "execute_tools" ambiguous, so the catalog refuses to serve rather than
	// silently picking the first match.
	dupErr string
	// encodeErr reports that an entry's input schema could not be encoded. It
	// only blocks "search_tools", which cannot describe a catalog it cannot
	// render; "execute_tools" can still run the tool.
	encodeErr bool
	// maxToolCalls caps the number of calls in one execute batch.
	maxToolCalls int
	// maxOutputBytes caps a catalog tool's reply, measured as that reply is
	// serialized.
	maxOutputBytes int
	// installed reports whether the two meta tools have been appended to the
	// server's tool list. They are installed by the first option that registers
	// a catalog entry, so a server that only configures limits (or registers no
	// entry at all) never advertises meta tools over an empty catalog.
	installed bool
	// configErr, when set, is a server misconfiguration message reported as a
	// tool error result by both meta tools. It is recorded once, after every
	// option has been applied.
	configErr string
}

// newToolCatalog returns an empty catalog for the server, carrying the default
// limits.
func newToolCatalog(s *Server) *toolCatalog {
	return &toolCatalog{
		server:         s,
		maxToolCalls:   defaultCatalogMaxToolCalls,
		maxOutputBytes: defaultCatalogMaxOutputBytes,
	}
}

// WithToolCatalog registers the given tools in the server's searchable tool
// catalog. Catalog tools are NOT listed by tools/list; the server exposes two
// meta tools in their place, "search_tools" (find catalog tools and their
// complete input schemas) and "execute_tools" (run a batch of them). This keeps
// a large tool surface out of the client's context while leaving every tool
// reachable.
//
// It may be called several times; each call appends to the same catalog. The
// meta tools are inserted into the tool list at the position of the first
// catalog option that registers an entry, so tools registered with WithTools
// keep their relative order. An option that registers nothing (no arguments, or
// only nil tools) installs no meta tools: a catalog that can never find or run
// anything is not advertised.
func WithToolCatalog(tools ...Tool) Option {
	return func(s *Server) {
		c := catalogFor(s)
		for _, t := range tools {
			if t != nil {
				c.tools = append(c.tools, t)
			}
		}
		if len(c.tools) > 0 && !c.installed {
			c.installed = true
			s.tools = append(s.tools, &catalogSearchTool{catalog: c}, &catalogExecuteTool{catalog: c})
		}
	}
}

// WithToolCatalogLimits sets the tool catalog's batch and output limits:
// maxToolCalls caps how many calls one execute_tools request may carry, and
// maxOutputBytes caps every reply either meta tool returns, measured on the
// bytes the client receives: the content items, the metadata and the flags of
// the complete tool result, serialized by the same encoder that puts them on
// the wire. Values below the floors (1 call, 512 bytes) are raised to them, so
// a zero or negative value yields the floor rather than an unusable catalog.
//
// What is measured is the tool result the meta tool produces. The reply
// envelope adds its own members to that result afterwards (the result type and
// the server implementation metadata), which the catalog cannot see and does
// not subtract: the result member a client receives is that many bytes wider
// than the limit, the same handful of bytes for every reply of a given server.
//
// It configures the catalog without populating it: on its own it registers no
// meta tools, so the limits apply whether it is applied before or after
// WithToolCatalog.
func WithToolCatalogLimits(maxToolCalls, maxOutputBytes int) Option {
	return func(s *Server) {
		c := catalogFor(s)
		c.maxToolCalls = max(minCatalogMaxToolCalls, maxToolCalls)
		c.maxOutputBytes = max(minCatalogMaxOutputBytes, maxOutputBytes)
	}
}

// catalogFor returns the server's catalog, creating it on first use so either
// catalog option can be applied first.
func catalogFor(s *Server) *toolCatalog {
	if s.catalog == nil {
		s.catalog = newToolCatalog(s)
	}
	return s.catalog
}

// finalizeToolCatalog checks the server's tool names once every option has been
// applied. The MCP specification requires tool names to be unique within a
// server, and the catalog contributes two generated names, so a collision is a
// server misconfiguration. New has no error return and library code never
// panics, so the collision is recorded on the catalog (both meta tools then
// report it as a tool error result) and logged when a logger is configured. The
// colliding tools stay in the list: dropping one silently would hide the
// misconfiguration instead of surfacing it.
func finalizeToolCatalog(s *Server) {
	c := s.catalog
	if c == nil || !c.installed {
		return
	}
	c.prepare()

	seen := make(map[string]struct{}, len(s.tools))
	for _, t := range s.tools {
		name := t.Name()
		if _, dup := seen[name]; dup {
			c.configErr = "Duplicate server tool name [" + name + "]."
			if s.logger != nil {
				s.logger.Error("mcp: duplicate server tool name", "tool", name)
			}
			return
		}
		seen[name] = struct{}{}
	}
}

// catalogEntry is one prepared catalog tool: the payload a search result
// carries, the encoded size that payload costs, and the case-folded text a
// query is matched against. Preparing an entry costs a schema build and a JSON
// encode, and the catalog never changes once the server has applied its
// options, so each entry is prepared once instead of on every search.
type catalogEntry struct {
	// tool is the registered tool the entry stands for.
	tool Tool
	// payload is the entry a search result carries.
	payload catalogToolPayload
	// size is the encoded byte size of payload, the unit the output limit is
	// measured in.
	size int
	// name, description and schemaText are the case-folded text a query term is
	// matched against, in descending search weight.
	name        string
	description string
	schemaText  string
	// nameTerms are the search terms of the tool name, compared against the
	// query terms for the exact-name boost.
	nameTerms []string
}

// prepare renders every catalog entry and records the checks a request would
// otherwise repeat: the duplicate-name check, the schema build, and the
// encoding of both the payload and the text a query is matched against. It runs
// once, after every option has been applied.
func (c *toolCatalog) prepare() {
	seen := make(map[string]struct{}, len(c.tools))
	for _, t := range c.tools {
		name := t.Name()
		if _, dup := seen[name]; dup {
			c.dupErr = "Duplicate tool name [" + name + "] in the tool catalog."
			return
		}
		seen[name] = struct{}{}
	}

	c.entries = make([]catalogEntry, 0, len(c.tools))
	for _, t := range c.tools {
		payload := catalogPayloadFor(t)
		schemaText, terr := catalogEncode(payload.InputSchema)
		size, serr := catalogWireSize(payload)
		if terr != nil || serr != nil {
			// The entry cannot be described; the search tool reports that
			// rather than ranking against a half-rendered payload.
			c.encodeErr = true
		}
		c.entries = append(c.entries, catalogEntry{
			tool:        t,
			payload:     payload,
			size:        size,
			name:        str.Lower(payload.Name),
			description: str.Lower(payload.Description),
			schemaText:  str.Lower(string(schemaText)),
			nameTerms:   catalogTerms(payload.Name),
		})
	}
}

// catalogToolPayload is one entry of a search result: the tool's exact name,
// description, and complete input schema, plus its behavior-hint annotations
// when it declares any. The payload is read by a model, so the fields are
// written in the order they are read in rather than alphabetically.
type catalogToolPayload struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
}

// catalogSearchOutput is the payload "search_tools" returns as JSON text: the
// matching catalog entries and whether more matched than were returned.
type catalogSearchOutput struct {
	OK      bool                 `json:"ok"`
	Tools   []catalogToolPayload `json:"tools"`
	HasMore bool                 `json:"hasMore"`
}

// catalogSearchLimitReport is the payload "search_tools" returns when not one
// matching entry can be reported within the catalog's output limit. A search
// runs no calls, so it reports nothing beyond the limit it could not meet.
type catalogSearchLimitReport struct {
	OK    bool              `json:"ok"`
	Error catalogLimitError `json:"error"`
}

// catalogExecuteReport is the payload "execute_tools" returns as JSON text. It
// has the same shape for every outcome, so a client reads one report whether
// the batch finished, stopped on a failing call, or could not carry a result.
//
// The content of the inner calls is not repeated here. Each result's content
// items are forwarded as content items of the batch result itself, in call
// order, directly after this report, and every record says how many of them its
// call contributed. Image and audio items, their audience annotations and their
// own metadata therefore reach the host in the channels it reads them from
// instead of being flattened into the text a model reads.
//
// The three counters say how far the batch got, and they are the authority on
// that: RequestedToolCalls is the size of the batch, AttemptedToolCalls how many
// of those calls were run, and ReportedToolCalls how many records this report
// carries. A call that was attempted without a record here is the one named by
// OmittedToolCall, whose result could not be carried; the calls from
// AttemptedToolCalls on never ran at all, either because an earlier call
// reported an error or because the output limit left no room to report another
// one.
//
// OK and Complete are two independent answers, and a client that reads one of
// them has read half the outcome. OK says whether the calls this report carries
// a record for all succeeded. Complete says whether the batch covered the whole
// request: every requested call was attempted, and every attempted call has a
// record here. A batch that stopped for room therefore reports ok with complete
// false, and the client reissues the calls from AttemptedToolCalls on. Complete
// restates what the three counters already say, so that an outcome a host reads
// through the two flags alone is never mistaken for a finished batch.
type catalogExecuteReport struct {
	OK                 bool                  `json:"ok"`
	Complete           bool                  `json:"complete"`
	Error              *catalogLimitError    `json:"error,omitempty"`
	Results            []catalogResultRecord `json:"results"`
	OmittedToolCall    *catalogOmittedCall   `json:"omittedToolCall,omitempty"`
	RequestedToolCalls int                   `json:"requestedToolCalls"`
	AttemptedToolCalls int                   `json:"attemptedToolCalls"`
	ReportedToolCalls  int                   `json:"reportedToolCalls"`
}

// catalogResultRecord is one record of an execute report: the call it answers,
// by name and by its zero-based position in the batch, whether that call
// reported an error, how many of the batch result's content items belong to it,
// and the structured content it returned.
type catalogResultRecord struct {
	Name              string         `json:"name"`
	Index             int            `json:"index"`
	IsError           bool           `json:"isError"`
	ContentItems      int            `json:"contentItems"`
	StructuredContent map[string]any `json:"structuredContent,omitempty"`
}

// catalogLimitError is the error object carried by a catalog report: the kind
// tells an output that did not fit from one that could not be rendered at all.
type catalogLimitError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// catalogOmittedCall identifies the one call of an execute batch whose result
// was left out of the payload, by name and by its zero-based position in the
// batch. It is the only call a client has to run again: it did run, so
// repeating it repeats whatever it changed. The name is the one the client
// supplied and may have been clipped to hold the report to the output limit;
// the index never is, so it is what identifies the call unambiguously.
type catalogOmittedCall struct {
	Name  string `json:"name"`
	Index int    `json:"index"`
}

// limitMessage names the output limit a reply could not be held to.
func (c *toolCatalog) limitMessage() string {
	return fmt.Sprintf("The tool output exceeded %d bytes.", c.maxOutputBytes)
}

// worstFailure returns the failure kind and message that cost the most bytes to
// report. A batch weighs the room a failure of its next call would need before
// running that call, and it cannot know yet which of the two failures it would
// be, so it reserves room for the larger one.
func (c *toolCatalog) worstFailure() (kind, message string) {
	limit := c.limitMessage()
	if len(catalogKindEncodeFailure)+len(catalogEncodeFailureMessage) >= len(catalogKindOutputLimit)+len(limit) {
		return catalogKindEncodeFailure, catalogEncodeFailureMessage
	}
	return catalogKindOutputLimit, limit
}

// messageReply builds an error result carrying message, held to the output
// limit like every other catalog reply: a diagnostic that broke the limit it
// exists to describe would be no better than the reply it replaced. A message
// the limit cannot carry is cut on a character boundary, so a multibyte message
// is never split mid-character, and marked as clipped.
func (c *toolCatalog) messageReply(message string) *Response {
	build := func(text string) (*Response, bool) {
		resp := Error(text)
		size, err := catalogReplySize(resp)
		return resp, err == nil && size <= c.maxOutputBytes
	}
	if resp, ok := build(message); ok {
		return resp
	}

	runes := []rune(message)
	kept := catalogLongestFit(len(runes), func(n int) bool {
		_, ok := build(catalogClip(runes, n))
		return ok
	})
	resp, ok := build(catalogClip(runes, kept))
	if ok || len(runes) == 0 {
		return resp
	}
	// Not even the truncation marker fits; an empty message is all the limit
	// leaves, and the error flag still says the call failed.
	resp, _ = build("")
	return resp
}

// encodeFailure is the error result returned when a catalog payload cannot be
// encoded. The message names no internal detail.
func (c *toolCatalog) encodeFailure() *Response {
	return c.messageReply(catalogEncodeFailureMessage)
}

// searchLimitExceeded builds the reply a search returns when its matches cannot
// be reported within the output limit. It is the smallest truthful answer the
// catalog has, and the output floor is set above it, so it is returned whatever
// it measures rather than being shrunk further.
func (c *toolCatalog) searchLimitExceeded() (*Response, error) {
	report := catalogSearchLimitReport{
		Error: catalogLimitError{Kind: catalogKindOutputLimit, Message: c.limitMessage()},
	}
	resp, _, err := catalogTextReply(report, true)
	return resp, err
}

// catalogClip renders the first n characters of a value, marking it as clipped
// when anything was left off.
func catalogClip(runes []rune, n int) string {
	if n >= len(runes) {
		return string(runes)
	}
	return string(runes[:n]) + catalogTruncationMarker
}

// catalogLongestFit returns the largest n in [0,length] for which fits reports
// true, or 0 when none does. A rendered reply grows with the number of
// characters kept, so the answer is found by bisection rather than by rendering
// once per character of a value that may be hundreds long.
func catalogLongestFit(length int, fits func(int) bool) int {
	lo, hi := 0, length
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if fits(mid) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// catalogTextReply renders a catalog payload as the JSON text of a tool result
// and measures that result as it is serialized.
func catalogTextReply(payload any, isError bool) (*Response, int, error) {
	text, err := catalogEncode(payload)
	if err != nil {
		return nil, 0, err
	}
	resp := Text(string(text))
	if isError {
		resp = resp.AsError()
	}
	size, err := catalogReplySize(resp)
	if err != nil {
		return nil, 0, err
	}
	return resp, size, nil
}

// catalogReplySize measures a reply as the client receives it: the complete
// tool result, with its content items, its metadata and its flags, serialized
// by the encoder that puts it on the wire. The output limit is applied to this
// number and to no other, so what a meta tool promises to hold to is what it
// actually sends; a size added up from the parts would leave out the envelope,
// the metadata channel and the escaping the text pays for once it is embedded
// in a content item.
func catalogReplySize(resp *Response) (int, error) {
	result, err := ToolResult(resp)
	if err != nil {
		return 0, err
	}
	return catalogWireSize(result)
}

// catalogBatchEntry is the outcome of one call of an execute batch, split into
// the channels a tool result carries: the bookkeeping the report states, the
// content items forwarded to the batch result, the structured content, and the
// metadata forwarded to the batch result's metadata channel.
type catalogBatchEntry struct {
	index      int
	name       string
	isError    bool
	content    []map[string]any
	structured map[string]any
	meta       map[string]any
}

// catalogBatchEntryFor splits one inner "tools/call" result into its channels.
// The result is the map InvokeTool builds, so every content item is already in
// the shape the MCP specification defines for a tool result and is carried
// through unchanged. An item that is not an object cannot be produced by that
// path; one is left out rather than guessed at, and the record's content count
// then states how many items really follow.
func catalogBatchEntryFor(index int, name string, result map[string]any) catalogBatchEntry {
	isError, _ := result["isError"].(bool)
	structured, _ := result["structuredContent"].(map[string]any)
	meta, _ := result["_meta"].(map[string]any)

	items, _ := result["content"].([]any)
	shapes := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if shape, ok := item.(map[string]any); ok {
			shapes = append(shapes, shape)
		}
	}

	return catalogBatchEntry{
		index:      index,
		name:       name,
		isError:    isError,
		content:    shapes,
		structured: structured,
		meta:       meta,
	}
}

// record renders the entry's bookkeeping for the report.
func (e catalogBatchEntry) record() catalogResultRecord {
	return catalogResultRecord{
		Name:              e.name,
		Index:             e.index,
		IsError:           e.isError,
		ContentItems:      len(e.content),
		StructuredContent: e.structured,
	}
}

// catalogContent is one content item of an inner tool result, carried into the
// batch result exactly as the inner call produced it. The shape is the one the
// specification defines for a "tools/call" content item, so forwarding it keeps
// an image, an audio clip, an audience annotation and an item's own metadata in
// the channel a host reads them from.
type catalogContent struct{ shape map[string]any }

// Compile-time assertion that a forwarded item is ordinary response content.
var _ content.Content = catalogContent{}

// MarshalJSON writes the forwarded shape.
func (c catalogContent) MarshalJSON() ([]byte, error) { return json.Marshal(c.shape) }

// ToTool returns the forwarded shape, which is already a tool result item.
func (c catalogContent) ToTool() (map[string]any, error) { return c.shape, nil }

// ToPrompt refuses: a forwarded tool result item has no prompt message shape.
func (c catalogContent) ToPrompt() (map[string]any, error) { return nil, content.ErrNotAllowed }

// ToResource refuses: a forwarded tool result item has no resource shape.
func (c catalogContent) ToResource(string, string) (map[string]any, error) {
	return nil, content.ErrNotAllowed
}

// String returns the text of a forwarded text item, or its type otherwise.
func (c catalogContent) String() string {
	if text, ok := c.shape["text"].(string); ok {
		return text
	}
	kind, _ := c.shape["type"].(string)
	return kind
}

// SetMeta sets one metadata key on the forwarded item.
func (c catalogContent) SetMeta(key string, value any) {
	c.MergeMeta(map[string]any{key: value})
}

// MergeMeta merges metadata into the forwarded item, keeping whatever the inner
// call already attached to it.
func (c catalogContent) MergeMeta(meta map[string]any) {
	if len(meta) == 0 || c.shape == nil {
		return
	}
	existing, _ := c.shape["_meta"].(map[string]any)
	merged := make(map[string]any, len(existing)+len(meta))
	for key, value := range existing {
		merged[key] = value
	}
	for key, value := range meta {
		merged[key] = value
	}
	c.shape["_meta"] = merged
}

// catalogBatch renders the outcome of an execute batch. Every reply the batch
// can return is built here and measured as it is serialized, so the report, the
// forwarded content and the metadata are weighed against the output limit
// together rather than a part of them being estimated.
type catalogBatch struct {
	catalog   *toolCatalog
	requested int
	entries   []catalogBatchEntry
}

// report builds the report for a batch carrying the results of every entry it
// holds. ok is false when the last of them reported an error, which stops the
// batch; a batch that stopped for room reports ok, since nothing it ran failed,
// and render marks it incomplete from its counters.
func (b *catalogBatch) report(ok bool) catalogExecuteReport {
	return catalogExecuteReport{
		OK:                 ok,
		RequestedToolCalls: b.requested,
		AttemptedToolCalls: len(b.entries),
	}
}

// render builds the reply for a report carrying kept and returns it with the
// size it serializes to. The report leads the content, the content items of
// each kept call follow it in call order, and the metadata of those calls
// travels in the reply's metadata channel.
//
// Every reply the batch can return is built here, so the record count and the
// completeness flag are derived from the counters here too: they can never
// disagree with them, and the flag is part of every reply that is weighed
// against the output limit rather than a member added to one afterwards.
func (b *catalogBatch) render(rep catalogExecuteReport, kept []catalogBatchEntry) (*Response, int, error) {
	rep.Results = make([]catalogResultRecord, 0, len(kept))
	items := 0
	for _, entry := range kept {
		rep.Results = append(rep.Results, entry.record())
		items += len(entry.content)
	}
	rep.ReportedToolCalls = len(kept)
	rep.Complete = rep.AttemptedToolCalls == rep.RequestedToolCalls &&
		rep.ReportedToolCalls == rep.AttemptedToolCalls

	text, err := catalogEncode(rep)
	if err != nil {
		return nil, 0, err
	}

	contents := make([]content.Content, 0, 1+items)
	contents = append(contents, content.NewText(string(text)))
	for _, entry := range kept {
		for _, shape := range entry.content {
			contents = append(contents, catalogContent{shape: shape})
		}
	}

	resp := NewResponse(contents...)
	if meta := catalogBatchMeta(kept); meta != nil {
		resp = resp.WithMeta(catalogMetaKey, meta)
	}
	if !rep.OK {
		resp = resp.AsError()
	}

	size, err := catalogReplySize(resp)
	if err != nil {
		return nil, 0, err
	}
	return resp, size, nil
}

// catalogBatchMeta collects the metadata of the inner results that carried any,
// keyed by the call it belongs to so a host can tell whose metadata it is
// reading. Nothing is returned when no call attached metadata, so a batch of
// ordinary results adds no metadata channel of its own.
func catalogBatchMeta(kept []catalogBatchEntry) map[string]any {
	results := make([]any, 0, len(kept))
	for _, entry := range kept {
		if len(entry.meta) == 0 {
			continue
		}
		results = append(results, map[string]any{
			"index": entry.index,
			"name":  entry.name,
			"meta":  entry.meta,
		})
	}
	if len(results) == 0 {
		return nil
	}
	return map[string]any{"results": results}
}

// failReply builds the reply for a batch whose call at index produced a result
// the batch cannot carry: one that does not encode, or one that would push the
// reply past the output limit. Every result the batch already holds is kept,
// because a call that changed something must never have to be run again to
// recover what it returned, and the named call is the only one a client has to
// repeat.
//
// The reply is then held to the limit it reports, since a report that broke
// that limit while announcing it would be no better than the reply it replaced.
// The only thing that gives way is the client-supplied name of the omitted
// call, which is clipped and marked: the index identifies that call just as
// well, and it is never clipped. The second return says whether the reply that
// came out fits the limit at all.
func (b *catalogBatch) failReply(kind, message, name string, index int) (*Response, bool) {
	rep := catalogExecuteReport{
		OK:                 false,
		Error:              &catalogLimitError{Kind: kind, Message: message},
		OmittedToolCall:    &catalogOmittedCall{Name: name, Index: index},
		RequestedToolCalls: b.requested,
		AttemptedToolCalls: index + 1,
	}

	build := func(called string) (*Response, bool) {
		resp, size, err := b.render(catalogRenamedOmitted(rep, called), b.entries)
		return resp, err == nil && size <= b.catalog.maxOutputBytes
	}
	if resp, ok := build(name); ok {
		return resp, true
	}

	runes := []rune(name)
	kept := catalogLongestFit(len(runes), func(n int) bool {
		_, ok := build(catalogClip(runes, n))
		return ok
	})
	resp, ok := build(catalogClip(runes, kept))
	if ok || len(runes) == 0 {
		return resp, ok
	}
	// Not even the truncation marker fits, so the name is dropped whole; the
	// index and the counters are what a client needs to resume the batch.
	return build("")
}

// canReport reports whether a failure of the call at index could be reported
// while keeping every result the batch already holds. The batch checks it
// before running that call, so a result that was produced is never dropped to
// make room for the diagnostics of a later one: a call whose outcome could not
// be reported is simply never started, and the counters say so.
//
// Which of the two failures it would be is not known yet, so the room reserved
// is for the costlier one.
func (b *catalogBatch) canReport(name string, index int) bool {
	kind, message := b.catalog.worstFailure()
	_, ok := b.failReply(kind, message, name, index)
	return ok
}

// failed builds the reply for a call whose result the batch cannot carry. The
// batch only runs a call once canReport has confirmed this reply fits.
func (b *catalogBatch) failed(kind, message, name string, index int) *Response {
	resp, _ := b.failReply(kind, message, name, index)
	return resp
}

// catalogRenamedOmitted returns the report with the omitted call renamed. The
// call is copied rather than mutated, so a candidate weighed against the limit
// never changes the report it was derived from.
func catalogRenamedOmitted(rep catalogExecuteReport, name string) catalogExecuteReport {
	renamed := *rep.OmittedToolCall
	renamed.Name = name
	rep.OmittedToolCall = &renamed
	return rep
}

// catalogSearchTool is the "search_tools" meta tool: it ranks the catalog
// against a query and returns the matching tools with their complete input
// schemas, so a client can call them through "execute_tools" without the server
// ever listing them.
type catalogSearchTool struct{ catalog *toolCatalog }

// Compile-time assertions that the search tool is a fully described primitive.
var (
	_ Tool      = (*catalogSearchTool)(nil)
	_ Titled    = (*catalogSearchTool)(nil)
	_ Annotated = (*catalogSearchTool)(nil)
)

// Name returns the fixed catalog search tool name.
func (t *catalogSearchTool) Name() string { return catalogSearchToolName }

// Title returns the human-friendly search tool title.
func (t *catalogSearchTool) Title() string { return "Search Tools" }

// Description returns the search tool description shown in tools/list.
func (t *catalogSearchTool) Description() string {
	return "Search the tools available through execute_tools. Returns exact tool names, descriptions, and complete input schemas. An empty query browses the catalog."
}

// Annotations hints that searching the catalog does not modify anything.
func (t *catalogSearchTool) Annotations() ToolAnnotations {
	readOnly := true
	return ToolAnnotations{ReadOnly: &readOnly}
}

// Schema declares the search arguments: an optional query and result limit.
func (t *catalogSearchTool) Schema(s *schema.Object) {
	s.String("query").Max(maxCatalogQueryChars).Description("Search terms. An empty query browses the catalog.")
	s.Integer("limit").Min(1).Max(maxCatalogSearchLimit).Description("Maximum results to return. Defaults to 10.")
}

// Handle validates the search arguments and returns the ranked catalog entries
// as a JSON text result.
func (t *catalogSearchTool) Handle(_ context.Context, req *Request) (*Response, error) {
	if resp := t.catalog.configResponse(); resp != nil {
		return resp, nil
	}
	if err := req.Validate(validation.Rules{
		"query": {validation.Nullable(), validation.String(), catalogQueryChars},
		"limit": {validation.Nullable(), validation.Integer(), validation.Between(1, maxCatalogSearchLimit)},
	}); err != nil {
		return t.catalog.messageReply(ValidationMessage(err)), nil
	}

	if t.catalog.dupErr != "" {
		return t.catalog.messageReply(t.catalog.dupErr), nil
	}
	if t.catalog.encodeErr {
		return t.catalog.encodeFailure(), nil
	}

	resp, err := t.catalog.search(req.String("query"), catalogSearchLimit(req))
	if err != nil {
		return t.catalog.encodeFailure(), nil
	}
	return resp, nil
}

// catalogSearchLimit resolves the validated "limit" argument, falling back to
// the default when the client did not supply one. The framework's integer rule
// accepts a decimal string as well as a JSON number, so a limit that passed
// validation is honored in either form rather than silently reverting to the
// default.
func catalogSearchLimit(req *Request) int {
	if n, ok := req.IntOK("limit"); ok {
		return int(n)
	}
	if s, ok := req.StringOK("limit"); ok {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return int(n)
		}
	}
	return defaultCatalogSearchLimit
}

// catalogQueryChars and catalogCallNameChars limit the two string arguments the
// meta tools accept to the "maxLength" they advertise, measured (like the
// keyword itself) in characters.
//
// Each is one rule value, reused on every request: a rule set carrying two
// distinct values under one rule name is refused when it is normalized, so the
// limit is part of the name.
var (
	catalogQueryChars    = catalogMaxChars(maxCatalogQueryChars)
	catalogCallNameChars = catalogMaxChars(maxCatalogCallNameChars)
)

// catalogMaxChars builds the maximum-length rule for a limit in characters. The
// framework's max rule measures a string in bytes, which would reject a
// multibyte value the advertised schema accepts, so the rule composes the
// framework's rune-aware length helper instead and runs in the same validator
// as every other rule on the field.
func catalogMaxChars(limit int) validation.Rule {
	return validation.Custom("mcp_max_chars_"+strconv.Itoa(limit), func(field string, value any, _ []string, _ map[string]any) error {
		text, ok := value.(string)
		if !ok || str.Length(text) <= limit {
			return nil
		}
		return errors.New(catalogLengthMessage(field, limit))
	})
}

// catalogLengthMessage renders the maximum-length failure for a field.
func catalogLengthMessage(field string, limit int) string {
	return "The " + field + " field must not exceed " + strconv.Itoa(limit) + " characters."
}

// search ranks the catalog against the query and returns the reply to send:
// the highest ranked entries that can be reported within the output limit, or
// the limit report when not one of them can.
//
// The reply is measured as it is serialized, so the number of entries it
// carries is settled by rendering it rather than by adding the prepared entry
// sizes up. Those sizes are a lower bound on that measurement, so they still
// bound the work: an entry that cannot fit even before the reply is wrapped is
// never rendered.
func (c *toolCatalog) search(query string, limit int) (*Response, error) {
	ranked := c.rank(query)

	fitting, lower := 0, 0
	for _, index := range ranked {
		if fitting >= limit {
			break
		}
		lower += c.entries[index].size
		if lower > c.maxOutputBytes {
			break
		}
		fitting++
	}

	for n := fitting; n >= 0; n-- {
		if n == 0 && len(ranked) > 0 {
			// Entries matched but not one of them fits, so the limit is all
			// there is left to report.
			break
		}
		out := catalogSearchOutput{
			OK:      true,
			Tools:   c.payloads(ranked[:n]),
			HasMore: n < len(ranked),
		}
		resp, size, err := catalogTextReply(out, false)
		if err != nil {
			return nil, err
		}
		if size <= c.maxOutputBytes {
			return resp, nil
		}
	}

	return c.searchLimitExceeded()
}

// rank scores the catalog against the query and returns the matching entry
// indexes, best first.
//
// Scoring is additive over the query terms: a query whose terms are exactly the
// tool's name terms takes a large head start, then each term scores again for
// appearing in the name, the description, and the rendered input schema, in
// descending weight. Equal scores keep registration order. An empty query
// scores nothing and browses the whole catalog.
func (c *toolCatalog) rank(query string) []int {
	terms := catalogTerms(query)

	type candidate struct {
		index int
		score int
	}

	candidates := make([]candidate, 0, len(c.entries))
	for i, entry := range c.entries {
		score := 0
		if len(terms) > 0 && slices.Equal(terms, entry.nameTerms) {
			score = 8
		}
		for _, term := range terms {
			if str.Contains(entry.name, term) {
				score += 4
			}
			if str.Contains(entry.description, term) {
				score += 2
			}
			if str.Contains(entry.schemaText, term) {
				score++
			}
		}

		if len(terms) > 0 && score == 0 {
			continue
		}
		candidates = append(candidates, candidate{index: i, score: score})
	}

	sort.SliceStable(candidates, func(a, b int) bool {
		if candidates[a].score != candidates[b].score {
			return candidates[a].score > candidates[b].score
		}
		return candidates[a].index < candidates[b].index
	})

	ranked := make([]int, 0, len(candidates))
	for _, cand := range candidates {
		ranked = append(ranked, cand.index)
	}
	return ranked
}

// payloads renders the search result entries for the given catalog indexes.
func (c *toolCatalog) payloads(indexes []int) []catalogToolPayload {
	out := make([]catalogToolPayload, 0, len(indexes))
	for _, index := range indexes {
		out = append(out, c.entries[index].payload)
	}
	return out
}

// catalogPayloadFor renders a catalog tool for a search result: its exact name,
// description, and complete input schema, plus its behavior-hint annotations
// when it declares any. The payload carries everything a client needs to build
// a well-formed call, and nothing it does not.
func catalogPayloadFor(t Tool) catalogToolPayload {
	obj := schema.NewObject()
	t.Schema(obj)
	input := obj.ToMap()
	if _, ok := input["properties"]; !ok {
		input["properties"] = map[string]any{}
	}

	payload := catalogToolPayload{
		Name:        t.Name(),
		Description: t.Description(),
		InputSchema: input,
	}
	if a, ok := t.(Annotated); ok {
		if annotations := a.Annotations().ToMap(); len(annotations) > 0 {
			payload.Annotations = annotations
		}
	}
	return payload
}

// catalogTerms splits text into case-folded search terms on runs of characters
// that are neither letters nor numbers, so punctuation, separators, and control
// characters all act as boundaries regardless of script.
func catalogTerms(text string) []string {
	return strings.FieldsFunc(str.Lower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

// catalogExecuteTool is the "execute_tools" meta tool: it runs a batch of
// catalog tools in order through the same invocation path a tools/call takes,
// and reports one result per call.
type catalogExecuteTool struct{ catalog *toolCatalog }

// Compile-time assertions that the execute tool is a fully described primitive.
var (
	_ Tool      = (*catalogExecuteTool)(nil)
	_ Titled    = (*catalogExecuteTool)(nil)
	_ Annotated = (*catalogExecuteTool)(nil)
)

// Name returns the fixed catalog execute tool name.
func (t *catalogExecuteTool) Name() string { return catalogExecuteToolName }

// Title returns the human-friendly execute tool title.
func (t *catalogExecuteTool) Title() string { return "Execute Tools" }

// Description returns the execute tool description shown in tools/list.
func (t *catalogExecuteTool) Description() string {
	return "Execute one or more independent catalog tools synchronously in order. Pass {\"calls\":[{\"name\":\"tool_name\",\"arguments\":{\"key\":\"value\"}}]} using exact names and arguments returned by search_tools. Execution stops on the first error. Check complete before treating the batch as done: when it is false the calls from attemptedToolCalls on never ran, so reissue them in a new execute_tools call. If a call depends on a previous result, invoke execute_tools again with a new calls array."
}

// Annotations hints that the batch may reach an open world of entities, since
// it runs whatever catalog tools the client names.
func (t *catalogExecuteTool) Annotations() ToolAnnotations {
	openWorld := true
	return ToolAnnotations{OpenWorld: &openWorld}
}

// Schema declares the batch argument: a non-empty list of {name, arguments}
// objects, capped at the catalog's per-batch limit.
func (t *catalogExecuteTool) Schema(s *schema.Object) {
	calls := s.Array("calls").
		Description("Independent tool calls to execute synchronously in order.").
		Min(1).
		Max(float64(t.catalog.maxToolCalls)).
		Required()

	item := calls.Items("object")
	props := item.Properties()
	props.String("name").
		Max(maxCatalogCallNameChars).
		Description("The exact tool name returned by search_tools.").
		Required()
	props.Object("arguments").Description("Arguments matching the tool input schema.")
	props.AdditionalProperties(false)
}

// Handle validates the batch, runs each call in order, and returns one reply
// describing every call that ran: a JSON report leading the content, the
// content items the calls produced following it in call order, and their
// metadata in the reply's metadata channel.
//
// The batch stops at the first call that reports an error, at the first result
// that cannot be rendered, before any call whose outcome the output limit could
// no longer be reported alongside the results already held, and after the call
// during which the request was cancelled, so an abandoned request launches no
// further tool. The reply is measured as the client receives it at every one of
// those points, so what the catalog sends is always within the limit it
// announces, and the results of the calls that already ran are never dropped to
// make room for the diagnostics of a later one. Every reply says which calls ran
// and which were never started, through the counters and through the
// completeness flag they are reduced to.
//
// When the client supplied a progress token, the batch reports its own progress
// under it: one notification per finished call, counted over the calls the
// batch was given.
func (t *catalogExecuteTool) Handle(ctx context.Context, req *Request) (*Response, error) {
	if resp := t.catalog.configResponse(); resp != nil {
		return resp, nil
	}
	if err := req.Validate(validation.Rules{
		"calls": {
			validation.Required(),
			validation.Array(),
			validation.Min(1),
			validation.Max(t.catalog.maxToolCalls),
		},
	}); err != nil {
		return t.catalog.messageReply(ValidationMessage(err)), nil
	}

	raw, _ := req.Get("calls").([]any)
	calls, err := catalogParseCalls(raw)
	if err != nil {
		// The message names every malformed entry, so a batch of them is as
		// long as the batch; it is held to the output limit like every other
		// reply rather than being the one answer that escapes it.
		return t.catalog.messageReply(ValidationMessage(err)), nil
	}

	if t.catalog.dupErr != "" {
		return t.catalog.messageReply(t.catalog.dupErr), nil
	}

	batch := &catalogBatch{catalog: t.catalog, requested: len(calls)}
	// The reply the batch holds from the start reports that nothing ran. Both
	// the advertised schema and the validator require a call, so an empty batch
	// cannot arrive from the wire, but a batch that cannot report even its
	// first call answers with this rather than with nothing at all.
	held, _, err := batch.render(batch.report(true), nil)
	if err != nil {
		return t.catalog.encodeFailure(), nil
	}

	for i, call := range calls {
		if !batch.canReport(call.name, i) {
			// Whatever this call returned could no longer be reported beside
			// the results already held, so it is not run at all, and the reply
			// held from the previous call is the answer. It was rendered with
			// this call still outstanding, so it already says so and was
			// measured saying so: stopping here costs no byte that was not
			// weighed against the limit.
			return held, nil
		}

		entry := catalogBatchEntryFor(i, call.name, t.catalog.invoke(ctx, req, call))
		batch.entries = append(batch.entries, entry)
		// One notification per finished call, counted over the batch: the
		// progress value rises by one for every notification sent under the
		// client's token, as the MCP progress notification requires, and only
		// reaches the total once the last call has run. Progress is
		// best-effort, so a sink failure never affects the batch result.
		_ = req.ReportProgress(ProgressUpdate{
			Progress: float64(i + 1),
			Total:    float64(len(calls)),
			Message:  "Ran [" + call.name + "].",
		})

		next, size, rerr := batch.render(batch.report(!entry.isError), batch.entries)
		if rerr != nil || size > t.catalog.maxOutputBytes {
			// The call ran; its result is what the reply cannot carry. It is
			// dropped and named, and the calls before it keep the results they
			// produced.
			kind, message := catalogKindOutputLimit, t.catalog.limitMessage()
			if rerr != nil {
				kind, message = catalogKindEncodeFailure, catalogEncodeFailureMessage
			}
			batch.entries = batch.entries[:i]
			return batch.failed(kind, message, call.name, i), nil
		}

		held = next
		if entry.isError {
			// The batch stops at the first failing call, and the reply it has
			// already carries every result the batch produced.
			return held, nil
		}
		if ctx.Err() != nil {
			// The request was abandoned while this call ran, so no further one
			// is started: the handler of a call that has not begun cannot
			// observe the cancellation itself, and a batch entry may change
			// something no client is waiting for any more. A single call is
			// still handed the abandoned context and left to decide, exactly as
			// it is on the direct path; what stops here is the launching of
			// work the client no longer asked for. The reply holds every result
			// the batch produced, and its counters say the rest never ran.
			return held, nil
		}
	}

	return held, nil
}

// catalogCall is one validated entry of an execute batch.
type catalogCall struct {
	name      string
	arguments map[string]any
}

// catalogParseCalls validates the shape of every batch entry and returns the
// calls to run. Entry-level failures are collected across the whole batch and
// returned as one validation error keyed by "calls.<index>[.field]", so a
// client sees every problem at once rather than one per round trip.
func catalogParseCalls(raw []any) ([]catalogCall, error) {
	fields := map[string][]string{}
	calls := make([]catalogCall, 0, len(raw))

	for i, item := range raw {
		key := "calls." + strconv.Itoa(i)

		obj, ok := item.(map[string]any)
		if !ok {
			fields[key] = append(fields[key], "The "+key+" field must be an object.")
			continue
		}
		for name := range obj {
			if name != "name" && name != "arguments" {
				// The message names no key, so it does not depend on Go's
				// randomized map iteration order.
				fields[key] = append(fields[key], "The "+key+" field must only contain the name and arguments keys.")
				break
			}
		}

		if err := NewRequest(obj).Validate(validation.Rules{
			"name": {validation.Required(), validation.String(), catalogCallNameChars},
		}); err != nil {
			catalogMergeFieldErrors(fields, key+".", err)
		}

		arguments := map[string]any{}
		switch v := obj["arguments"].(type) {
		case nil:
		case map[string]any:
			arguments = v
		default:
			fields[key+".arguments"] = append(fields[key+".arguments"], "The "+key+".arguments field must be an object.")
			continue
		}

		name, _ := obj["name"].(string)
		calls = append(calls, catalogCall{name: name, arguments: arguments})
	}

	if len(fields) > 0 {
		return nil, fmt.Errorf("%w: %w", ErrValidation, contract.ValidationErrors{Errors: fields})
	}
	return calls, nil
}

// catalogMergeFieldErrors folds the field messages of a validation error into
// fields under the given key prefix.
func catalogMergeFieldErrors(fields map[string][]string, prefix string, err error) {
	var verr contract.ValidationErrors
	if !errors.As(err, &verr) {
		return
	}
	for field, messages := range verr.Errors {
		key := prefix + field
		for _, message := range messages {
			// The validator names the bare field; re-key it so the client can
			// tell which batch entry failed.
			fields[key] = append(fields[key], str.ReplaceFirst("The "+field+" ", "The "+key+" ", message))
		}
	}
}

// invoke runs one catalog call and returns its tools/call result map. Every
// failure is contained in the result (isError:true) rather than propagated, so
// one bad call in a batch never takes down the batch response itself.
//
// The call takes the same path as a direct tools/call: it runs through the
// shared InvokeTool helper, so arguments are validated by the tool, a
// validation failure becomes a tool-level error result, and the response is
// serialized with its structured content and _meta. Session id, request _meta
// and the inbound request context are inherited from the batch request, so a
// tool moved behind the catalog resolves the same authenticated Request.User it
// would on the direct path. Each inner call also dispatches its own
// ToolCalled/ToolFailed event, so a tool moved behind the catalog stays as
// observable as one called directly.
//
// The inner request gets no streaming sink, which leaves its ReportProgress a
// no-op: the batch is one request and owns the client's progress token, so it
// reports its own progress over the calls (see catalogExecuteTool.Handle).
// Forwarding each inner tool's sequence under that one token instead would
// restart the count at every call, and the MCP progress notification requires
// the progress value to increase with every notification for a given token.
func (c *toolCatalog) invoke(ctx context.Context, parent *Request, call catalogCall) map[string]any {
	var target Tool
	for _, entry := range c.entries {
		if entry.payload.Name == call.name {
			target = entry.tool
			break
		}
	}
	if target == nil {
		// An unknown name is a failed call, exactly as it is on the direct
		// path, where it never reaches a handler either.
		c.dispatchFailed(ctx, parent, call, nil, 0)
		return catalogFailedResult("Tool [" + call.name + "] was not found in the catalog.")
	}

	req := NewRequest(call.arguments).
		WithSessionID(parent.SessionID()).
		WithMeta(parent.Meta()).
		WithRequestContext(parent.ctx)

	start := time.Now()
	result, err := InvokeTool(ctx, target, req)
	elapsed := time.Since(start)
	if err != nil {
		// The batch contains the failure instead of aborting, but it is still
		// reported: the event and the log carry the underlying error, while the
		// client only sees a message the protocol already deems safe.
		c.dispatchFailed(ctx, parent, call, err, elapsed)
		c.logError("mcp: catalog tool call failed", "tool", call.name, "error", err.Error())
		return catalogFailedResult(catalogFailureMessage(err))
	}

	isError, _ := result["isError"].(bool)
	c.dispatch(ctx, event.ToolCalled{
		SessionID: parent.SessionID(),
		Tool:      call.name,
		Arguments: call.arguments,
		IsError:   isError,
		Duration:  elapsed,
	})
	return result
}

// dispatchFailed emits the ToolFailed event for an inner call that never
// produced a result.
func (c *toolCatalog) dispatchFailed(ctx context.Context, parent *Request, call catalogCall, err error, elapsed time.Duration) {
	c.dispatch(ctx, event.ToolFailed{
		SessionID: parent.SessionID(),
		Tool:      call.name,
		Arguments: call.arguments,
		Err:       err,
		Duration:  elapsed,
	})
}

// dispatch emits an event through the owning server's dispatcher.
func (c *toolCatalog) dispatch(ctx context.Context, ev any) {
	if c.server == nil {
		return
	}
	c.server.dispatch(ctx, ev)
}

// logError reports a server-side failure through the configured logger, if any.
func (c *toolCatalog) logError(message string, args ...any) {
	if c.server == nil || c.server.logger == nil {
		return
	}
	c.server.logger.Error(message, args...)
}

// catalogFailureMessage renders a contained handler failure for the client. A
// *jsonrpc.Error carries the message a direct call would put on the wire, so it
// is preserved; anything else is reported with the same generic wording a
// direct call's internal error carries, so no internal detail leaks.
func catalogFailureMessage(err error) string {
	var rpcErr *jsonrpc.Error
	if errors.As(err, &rpcErr) && rpcErr != nil && rpcErr.Message != "" {
		return rpcErr.Message
	}
	return internalErrorMessage
}

// catalogFailedResult builds a tool-level error result carrying a single text
// message.
func catalogFailedResult(message string) map[string]any {
	result, _ := ToolResult(Error(message))
	return result
}

// configResponse returns the misconfiguration error result when the server's
// tool names collide, or nil when the catalog is usable.
func (c *toolCatalog) configResponse() *Response {
	if c.configErr == "" {
		return nil
	}
	return c.messageReply(c.configErr)
}

// catalogEncode renders a catalog payload as the compact JSON text a meta tool
// returns in a content item. HTML escaping is disabled so markup a tool
// produced reaches the model as it stands rather than as escape sequences; the
// text is a payload, not the measurement, and the reply that carries it is
// measured with the encoder that puts it on the wire.
func catalogEncode(payload any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return nil, err
	}
	// Encode terminates the value with a newline; the payload itself does not
	// carry one.
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// catalogWireSize returns the serialized byte size of a value, the unit the
// output limit is measured in. It encodes exactly as the transport does, so a
// reply weighed against the limit is weighed as the bytes the client receives:
// markup and line separators are escape sequences there, six bytes each, and a
// measurement taken with any other encoder would undercount a tool that returns
// HTML, XML or code by up to six times.
func catalogWireSize(v any) (int, error) {
	encoded, err := json.Marshal(v)
	if err != nil {
		return 0, err
	}
	return len(encoded), nil
}
