package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/velocitykode/prism"
	velapp "github.com/velocitykode/velocity/app"
	"github.com/velocitykode/velocity/str"

	"github.com/velocitykode/velocity-mcp/server"
)

const (
	// inspectorPackage is the npm package the MCP Inspector is launched from.
	// The version is pinned so every run drives the same inspector build
	// regardless of the npm cache on the machine; override it with
	// --inspector-version when testing against another release.
	//
	// The pin is part of the launch contract implemented below: this command
	// hands the inspector a configuration file on its own ("--config" with no
	// "--server") whose entry carries a transport "type", either a stdio
	// "command"/"args" pair or an http "url", and a "protocolEra". Inspector
	// majors before inspectorMinimumMajor reject "--config" unless "--server"
	// names an entry, and read only "command"/"args"/"env" from that entry, so
	// they cannot run this launch at all. Move the pin within that major range
	// only, or change the launch to match the version that replaces it.
	inspectorPackage = "@modelcontextprotocol/inspector"
	inspectorVersion = "2.7.0"

	// inspectorMinimumMajor is the oldest inspector major that understands the
	// configuration this command writes. --inspector-version is checked against
	// it so an override cannot quietly produce a launch the inspector refuses.
	inspectorMinimumMajor = 2

	// inspectorProtocolEra selects the protocol era the inspector negotiates in.
	// The server serves both handshake families: the "initialize" exchange used
	// up to 2025-11-25 and the "server/discover" exchange introduced in
	// 2026-07-28 (server.HandshakeFor). "auto" lets the inspector find which of
	// them the session ends up on instead of forcing one, so a launch works
	// whichever era the inspector build prefers; pinning "modern" would rule out
	// the older exchange for no gain. Any change here stays within the eras the
	// server can actually negotiate
	// (TestInspectorProtocolEraMatchesTheServerProtocol guards this).
	inspectorProtocolEra = "auto"

	// startCommandName is the command the inspector launches for a stdio
	// session; it is served by startCommand in this package.
	startCommandName = "mcp:start"
)

// inspectorShutdownGrace is how long the inspector is given to shut its own
// node children down after an interrupt before it is killed outright. It is a
// variable so a test can shorten the wait for a child that ignores the
// interrupt.
var inspectorShutdownGrace = 5 * time.Second

// inspectorSignalArrival is how long a run whose inspector was ended by a stop
// signal waits for the command's own copy of that signal. A terminal signals
// the whole job at once, and the inspector can be gone before the command has
// been handed its copy; the run must not report a failed inspector for what
// is a stop still on its way. It is a variable so a test can shorten the wait
// for a signal that never comes.
var inspectorSignalArrival = time.Second

// inspectorLaunch is the resolved description of the inspector process a run
// would start: the executable, its arguments, the environment overlaid on the
// current process environment, and the generated configuration file. Handle
// builds one and hands it to the command's runner, so a test can assert the
// exact wiring without ever spawning npx.
type inspectorLaunch struct {
	Name       string
	Args       []string
	Env        map[string]string
	ConfigPath string
}

// processRunner starts an inspector process. The default implementation shells
// out to npx; tests substitute a recorder.
type processRunner func(ctx context.Context, l inspectorLaunch) error

// prompter asks the operator for one value and returns the answer.
type prompter func(question string) string

// inspectorCommand launches the MCP Inspector against the served server: over
// stdio by re-running this application's mcp:start command (the default), or
// over streamable HTTP against a url given with --url. It is the interactive
// counterpart of inspectCommand, which only prints the primitive inventory.
type inspectorCommand struct {
	srv    *server.Server
	out    io.Writer
	run    processRunner
	ask    prompter
	binary func() (string, error)
}

// Name implements chain.Command.
func (inspectorCommand) Name() string { return "mcp:inspector" }

// Description implements chain.Command.
func (inspectorCommand) Description() string {
	return "Open the MCP Inspector to debug and test the MCP server"
}

// Handle resolves the transport configuration, writes it to a temporary
// inspector configuration file, prints the connection guidance, and runs the
// inspector until it exits or the process is signalled to stop.
func (c inspectorCommand) Handle(s *velapp.Services, args []string) error {
	if c.srv == nil {
		return errors.New("mcp: inspector command built without a server")
	}

	opts, err := parseInspectorArgs(args)
	if err != nil {
		return err
	}

	stoppedBy, err := c.inspect(opts)
	if err != nil {
		return err
	}
	// The run has ended what it started and removed its configuration file
	// by now, so a signal that ends the command itself may do so.
	return endAsSignalled(stoppedBy)
}

// stoppedBySignal is the cancellation cause of a run that a signal stopped.
// It tells the runner which signal to pass on to the inspector.
type stoppedBySignal struct{ sig os.Signal }

func (s stoppedBySignal) Error() string { return "stopped by signal: " + s.sig.String() }

// inspect performs one inspector run and reports the signal that stopped it,
// if one did. Everything the run leaves behind is cleaned up when it returns.
func (c inspectorCommand) inspect(opts inspectorOptions) (os.Signal, error) {
	// Announced before anything else happens, so the operator knows which
	// server they are answering route parameter questions for.
	if err := c.writeHeader(); err != nil {
		return nil, err
	}

	config, guidance, env, err := c.transportConfig(opts)
	if err != nil {
		return nil, err
	}

	configPath, err := writeInspectorConfig(c.srv.Name(), config)
	if err != nil {
		return nil, err
	}
	// The inspector reads the file at startup; it is ours to clean up, and it
	// may carry a url with credentials in it, so it does not outlive the run.
	defer func() { _ = os.Remove(configPath) }()

	guidance = append(guidance,
		[2]string{"Protocol Era", inspectorProtocolEra},
		[2]string{"Config", filepath.ToSlash(configPath)},
	)
	if err := c.writeGuidance(guidance); err != nil {
		return nil, err
	}

	// Every signal that ends the command is caught, a hangup and a quit
	// included: whichever it is, the run has to end the inspector and remove
	// the configuration file rather than leave both behind with the command
	// gone.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, inspectorStopSignals()...)
	defer signal.Stop(signals)

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case sig := <-signals:
			cancel(stoppedBySignal{sig: sig})
		case <-finished:
		}
	}()

	run := c.run
	if run == nil {
		run = runInspectorProcess
	}
	err = run(ctx, inspectorLaunch{
		Name:       "npx",
		Args:       []string{opts.packageSpec(), "--config", configPath},
		Env:        env,
		ConfigPath: configPath,
	})

	var stop stoppedBySignal
	if errors.As(context.Cause(ctx), &stop) {
		return stop.sig, err
	}
	return nil, err
}

// transportConfig builds the inspector's server entry plus the guidance lines
// and environment overlay that go with it. Without --url the server is
// inspected over stdio by re-running mcp:start, which is how an MCP client
// launches it in production.
func (c inspectorCommand) transportConfig(opts inspectorOptions) (map[string]any, [][2]string, map[string]string, error) {
	env := map[string]string{}
	if opts.host != "" {
		env["HOST"] = opts.host
	}
	if opts.port != "" {
		env["CLIENT_PORT"] = opts.port
	}
	if opts.caCert != "" {
		// The inspector runs under node, which reads extra trust anchors from
		// this variable and keeps verifying every certificate against them plus
		// its own store. It adds trust rather than removing it, so a session
		// against a development server stays protected from interception.
		env["NODE_EXTRA_CA_CERTS"] = opts.caCert
	}

	if opts.url == "" {
		// --command names the binary outright, which is also the way out when
		// the project binary cannot be located, so it is honoured before any
		// resolution is attempted.
		bin := opts.command
		if bin == "" {
			resolve := c.binary
			if resolve == nil {
				resolve = projectBinary
			}
			var err error
			if bin, err = resolve(); err != nil {
				return nil, nil, nil, err
			}
		}
		args := []string{"run", startCommandName}
		config := map[string]any{
			"type":        "stdio",
			"command":     bin,
			"args":        args,
			"protocolEra": inspectorProtocolEra,
		}
		guidance := [][2]string{
			{"Transport Type", "STDIO"},
			{"Command", filepath.ToSlash(bin)},
			{"Arguments", strings.Join(args, " ")},
		}
		// A stdio session carries no certificate of its own, but the inspector
		// still reaches an authorization server over HTTPS, so an extra anchor
		// is reported when one was given.
		if opts.caCert != "" {
			guidance = append(guidance, certificateGuidance(opts.caCert, ""))
		}
		return config, guidance, env, nil
	}

	parsed, err := c.serverURL(opts.url)
	if err != nil {
		return nil, nil, nil, err
	}
	serverURL := parsed.String()
	guidance := [][2]string{
		{"Transport Type", "Streamable HTTP"},
		{"URL", redactedURL(parsed)},
	}
	if parsed.Scheme == "https" || opts.caCert != "" {
		guidance = append(guidance, certificateGuidance(opts.caCert, parsed.Hostname()))
	}
	config := map[string]any{
		"type":        "http",
		"url":         serverURL,
		"protocolEra": inspectorProtocolEra,
	}
	return config, guidance, env, nil
}

// certificateGuidance is the line that tells the operator what the inspector
// will make of the server certificate. Verification is never switched off: the
// inspector process talks to more than the inspected server (the authorization
// server of an OAuth-protected session, npm, the registry), and node applies
// its certificate settings to every one of those connections, so an exception
// made for a local server would follow the session everywhere. A development
// certificate is trusted by naming its issuing CA with --ca-cert instead, which
// adds an anchor rather than removing verification.
func certificateGuidance(caCert, host string) [2]string {
	switch {
	case caCert != "":
		return [2]string{"Certificates", "verified against the system anchors and " + filepath.ToSlash(caCert)}
	case isLocalHost(host):
		return [2]string{"Certificates", "the server certificate must chain to a trusted CA; for a development certificate pass --ca-cert with its issuing CA"}
	default:
		return [2]string{"Certificates", "the server certificate must chain to a trusted CA"}
	}
}

// isLocalHost reports whether host names a server on the developer's own
// machine: the loopback interface, or one of the reserved development domains
// browsers and local proxies resolve to it. It selects the wording of the
// certificate guidance, because a locally issued certificate is the one that
// usually needs an anchor of its own. The mDNS suffix ".local" is deliberately
// absent, because it names any machine on the local network rather than this
// one.
func isLocalHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	if host == "localhost" {
		return true
	}
	for _, suffix := range []string{".localhost", ".test", ".localdomain"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// serverURL fills in every {parameter} placeholder of raw by asking for its
// value, then validates the result as an absolute http(s) url. A parameter left
// blank fails the run rather than producing a url that cannot resolve.
func (c inspectorCommand) serverURL(raw string) (*url.URL, error) {
	filled, err := c.fillRouteParameters(raw)
	if err != nil {
		return nil, err
	}

	// None of the refusals below shows the url or the parser's reason. The
	// parser quotes the url whole, and its reasons quote the part it stopped
	// at (a port, an escape), which in a url with credentials is a piece of
	// them. A url that is refused did not take the shape redactedURL relies
	// on to find the secrets either: without the scheme and host it expects,
	// a password or a token parses as a scheme or a path, which are shown.
	parsed, err := url.Parse(filled)
	if err != nil {
		return nil, errors.New("mcp: --url is not a valid url" + argumentWithheld)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("mcp: --url must be an absolute http or https url" + argumentWithheld)
	}
	if parsed.Host == "" {
		return nil, errors.New("mcp: --url is missing a host" + argumentWithheld)
	}
	return parsed, nil
}

// argumentWithheld ends every refusal of a command line argument. No error of
// this command shows the value it refuses: the operator can put a url with
// credentials after any flag, or after none, and the error goes to a terminal
// with scrollback and to CI logs. The flag is named instead, which is enough
// to find the value on the command line that was just typed.
const argumentWithheld = " (the value is not shown, since an argument may carry a url with credentials)"

// urlSecretMarker stands in for every part of a url that may carry a secret
// when the url is shown to the operator. It is the marker the standard library
// uses for a redacted password.
const urlSecretMarker = "xxxxx"

// redactedURL renders a url for the terminal and for error messages: the
// scheme, host and path as written, and a marker in place of the user
// information, of every query value, of the fragment, and of the opaque part
// of a url that has no authority. A secret is handed to this command in any
// of those places (a password, a bearer token in the user name, an api key in
// a query parameter, a token in the fragment), and the terminal is scrollback
// and CI log; the inspector itself reads the url whole from the configuration
// file. The path and the query keys stay visible: the path is the route the
// operator is checking, with the parameters they were just asked for filled
// in, and the keys say which parameters were passed. A bare key with no value
// is treated as a value, since a token is passed that way too. Parameters are
// told apart at "&" and at ";" alike: servers have split a query at either,
// and reading "token;x=1" as one parameter would show the token as its key.
//
// The path and the keys are shown only where they are plain text. A path
// segment or a key holding anything but letters, digits, ".", "_", "~" and
// "-" is replaced by the marker: an escape or a delimiter there (";", "=",
// "%3F") is where another parser, the server's among them, may start a
// parameter that this one read as part of a name, and a value hidden that way
// would otherwise be shown as route.
//
// It is only used for a url that serverURL accepted: an absolute http or https
// url with a host. Anything else is not shown at all (see argumentWithheld),
// because the parts named above are not where its secrets end up.
func redactedURL(u *url.URL) string {
	shown := *u
	if shown.User != nil {
		shown.User = url.User(urlSecretMarker)
	}
	if shown.Opaque != "" {
		shown.Opaque = urlSecretMarker
	}
	if path := u.EscapedPath(); path != "" {
		segments := strings.Split(path, "/")
		for i, segment := range segments {
			if !isPlainURLText(segment) {
				segments[i] = urlSecretMarker
			}
		}
		shown.Path, shown.RawPath = strings.Join(segments, "/"), ""
	}
	if shown.RawQuery != "" {
		shown.RawQuery = redactedQuery(shown.RawQuery)
		shown.ForceQuery = false
	}
	if shown.Fragment != "" || shown.RawFragment != "" {
		shown.Fragment, shown.RawFragment = urlSecretMarker, ""
	}
	return shown.String()
}

// isPlainURLText reports whether a path segment or a query key is made of
// unreserved url characters only, which no parser reads as a delimiter or
// decodes into one. The empty text is plain.
func isPlainURLText(text string) bool {
	for _, r := range text {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '.', r == '_', r == '~', r == '-':
		default:
			return false
		}
	}
	return true
}

// redactedQuery renders a query string with every parameter's value replaced
// by the marker, keeping the plain keys and the separators as written. A
// parameter with no "=" is replaced whole, as is a key that is not plain text,
// and an empty parameter (two separators in a row) stays empty.
func redactedQuery(query string) string {
	var b strings.Builder
	for len(query) > 0 {
		end := strings.IndexAny(query, "&;")
		parameter, separator := query, ""
		if end >= 0 {
			parameter, separator = query[:end], query[end:end+1]
		}
		switch key, _, hasValue := strings.Cut(parameter, "="); {
		case hasValue && !isPlainURLText(key):
			b.WriteString(urlSecretMarker + "=" + urlSecretMarker)
		case hasValue:
			b.WriteString(key + "=" + urlSecretMarker)
		case parameter != "":
			b.WriteString(urlSecretMarker)
		}
		b.WriteString(separator)
		query = query[len(parameter)+len(separator):]
	}
	return b.String()
}

// userInfoEnd reports where the user information of a url ends in raw: the
// index just past the last "@" of its first part, the one before any "/", "?"
// or "#" that follows the scheme. It is zero for a url that carries none.
// The part is found by its text rather than by parsing, because it is read
// before the route parameters are filled in and has to hold for a url the
// parser would refuse.
func userInfoEnd(raw string) int {
	start := 0
	if i := strings.Index(raw, "//"); i >= 0 && !strings.ContainsAny(raw[:i], "/?#") {
		start = i + 2
	}
	end := len(raw)
	if i := strings.IndexAny(raw[start:], "/?#"); i >= 0 {
		end = start + i
	}
	if at := strings.LastIndexByte(raw[start:end], '@'); at >= 0 {
		return start + at + 1
	}
	return 0
}

// fillRouteParameters replaces each {name} placeholder of a route path with a
// value supplied by the operator. A trailing ":constraint" inside the braces is
// part of the route declaration, not of the name, and the constraint is a regex
// that may carry braces of its own, as in "{id:[0-9]{2}}". Values are
// percent-encoded so an answer containing a slash or a space cannot reshape the
// url.
//
// The user information of the url is not searched. A route has no parameter
// there, and a password is free to hold braces: read as a placeholder, the
// text between them would be printed in the question and then replaced with
// the answer.
func (c inspectorCommand) fillRouteParameters(raw string) (string, error) {
	ask := c.ask
	if ask == nil {
		ask = promptForValue
	}

	var b strings.Builder
	skipped := userInfoEnd(raw)
	b.WriteString(raw[:skipped])
	rest := raw[skipped:]
	for {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			b.WriteString(rest)
			return b.String(), nil
		}
		end := routeParameterEnd(rest, open)
		if end < 0 {
			return "", errors.New("mcp: unterminated route parameter in --url" + argumentWithheld)
		}

		name := rest[open+1 : end]
		if i := strings.IndexByte(name, ':'); i >= 0 {
			name = name[:i]
		}
		if strings.TrimSpace(name) == "" {
			return "", errors.New("mcp: unnamed route parameter in --url" + argumentWithheld)
		}

		value := strings.TrimSpace(ask(fmt.Sprintf("What is the value for the [%s] route parameter?", name)))
		if value == "" {
			return "", errors.New("mcp: every route parameter needs a value to inspect this server")
		}

		b.WriteString(rest[:open])
		b.WriteString(url.PathEscape(value))
		rest = rest[end+1:]
	}
}

// routeParameterEnd reports the index of the brace closing the placeholder that
// opens at open, or -1 when the placeholder is never closed. A regex constraint
// brings its own braces: a quantifier such as "{2}" nests inside the
// placeholder, and a character class such as "[{]" carries one that is not a
// delimiter at all, so neither may be mistaken for the end. A placeholder never
// spans more than one path segment, which bounds the search.
func routeParameterEnd(s string, open int) int {
	var depth int
	var inClass bool
	for i := open; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\\':
			i++ // an escaped character never delimits anything
		case inClass:
			if c == ']' {
				inClass = false
			}
		case c == '[':
			inClass = true
		case c == '{':
			depth++
		case c == '}':
			if depth--; depth == 0 {
				return i
			}
		case c == '/' || c == '?' || c == '#':
			return -1
		}
	}
	return -1
}

// writeHeader announces the run before any question is asked or any process is
// started.
func (c inspectorCommand) writeHeader() error {
	bw := &errWriter{w: c.writer()}
	bw.printf("Starting the MCP Inspector for server [%s]\n", c.srv.Name())
	return bw.err
}

// writeGuidance prints the connection details the operator needs if they have to
// wire the inspector up by hand.
func (c inspectorCommand) writeGuidance(guidance [][2]string) error {
	bw := &errWriter{w: c.writer()}
	for _, line := range guidance {
		bw.printf("%s => %s\n", line[0], line[1])
	}
	bw.printf("\n")
	return bw.err
}

// writer is where the command reports to, defaulting to standard output.
func (c inspectorCommand) writer() io.Writer {
	if c.out == nil {
		return os.Stdout
	}
	return c.out
}

// inspectorOptions holds the parsed command line of mcp:inspector.
type inspectorOptions struct {
	url     string
	host    string
	port    string
	command string
	version string
	// caCert is the absolute path of a PEM file whose certificates the
	// inspector trusts in addition to the system anchors, which is how a
	// development certificate is accepted without weakening verification.
	caCert string
}

// packageSpec returns the version-pinned npm package argument for npx.
func (o inspectorOptions) packageSpec() string {
	version := o.version
	if version == "" {
		version = inspectorVersion
	}
	return inspectorPackage + "@" + version
}

// parseInspectorArgs reads the mcp:inspector flags. Every value ends up in an
// argument vector or an environment variable of a child process, so each one is
// validated here rather than trusted.
func parseInspectorArgs(args []string) (inspectorOptions, error) {
	var opts inspectorOptions
	targets := map[string]*string{
		"--url":               &opts.url,
		"--host":              &opts.host,
		"--port":              &opts.port,
		"--command":           &opts.command,
		"--inspector-version": &opts.version,
		"--ca-cert":           &opts.caCert,
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, inline, hasInline := strings.Cut(arg, "=")
		target, ok := targets[name]
		switch {
		case !ok && strings.HasPrefix(arg, "-"):
			if !isFlagName(name) {
				return inspectorOptions{}, fmt.Errorf("unknown flag at position %d%s", i+1, argumentWithheld)
			}
			return inspectorOptions{}, fmt.Errorf("unknown flag %q%s", name, argumentWithheld)
		case !ok:
			// A url passed without its flag is the likely stray argument, so
			// it is pointed at by position rather than shown.
			return inspectorOptions{}, fmt.Errorf("unexpected argument at position %d%s (usage: vel run mcp:inspector [--url URL] [--host HOST] [--port PORT] [--ca-cert FILE])", i+1, argumentWithheld)
		case hasInline:
			*target = inline
		case i+1 >= len(args):
			return inspectorOptions{}, fmt.Errorf("%s requires a value", name)
		default:
			*target = args[i+1]
			i++
		}
	}

	if err := validateEnvValue("--host", opts.host); err != nil {
		return inspectorOptions{}, err
	}
	if opts.port != "" {
		port, err := strconv.Atoi(opts.port)
		if err != nil || port < 1 || port > 65535 {
			return inspectorOptions{}, errors.New("--port must be a number between 1 and 65535" + argumentWithheld)
		}
	}
	if opts.version != "" {
		if !isVersionSpec(opts.version) {
			return inspectorOptions{}, errors.New("--inspector-version must be an npm version or tag" + argumentWithheld)
		}
		// A dist-tag cannot be resolved without reaching npm, so only an
		// explicit version is checked against the launch contract.
		if major, ok := versionMajor(opts.version); ok && major < inspectorMinimumMajor {
			return inspectorOptions{}, fmt.Errorf("--inspector-version must be %d.0.0 or newer, got major version %d: older inspectors do not understand the generated configuration file", inspectorMinimumMajor, major)
		}
	}
	if opts.command != "" && strings.ContainsAny(opts.command, "\x00\n\r") {
		return inspectorOptions{}, errors.New("--command must not contain control characters")
	}
	// The command is printed in the guidance and written to the configuration
	// file as the program to run. A url there is a value meant for --url, and
	// it would be shown with whatever credentials it carries.
	if looksLikeURL(opts.command) {
		return inspectorOptions{}, errors.New("--command must name a program, not a url; pass a url with --url" + argumentWithheld)
	}
	if opts.caCert != "" {
		path, err := caCertPath(opts.caCert)
		if err != nil {
			return inspectorOptions{}, err
		}
		opts.caCert = path
	}
	return opts, nil
}

// looksLikeURL reports whether a value given as the program to run is a url
// by its text: it names a scheme followed by "//", or starts with the http or
// https scheme in any form the url parser accepts, the one without slashes
// included. No program is named that way.
func looksLikeURL(value string) bool {
	if strings.Contains(value, "://") {
		return true
	}
	lower := str.Lower(value)
	return str.StartsWith(lower, "http:", "https:")
}

// isFlagName reports whether name reads as the name of a flag: letters,
// digits, dashes and underscores only. An unknown flag is quoted in its error
// only when it does. Anything else is a value typed where a flag was expected
// (a url always holds a character a flag name cannot), and it is pointed at
// by position rather than shown in part.
func isFlagName(name string) bool {
	for _, r := range name {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// caCertPath validates a --ca-cert value and resolves it to an absolute path.
// The file becomes an additional trust anchor of the inspector process, so a
// value naming no readable file is refused here: node ignores a path it cannot
// read, and the run would fail later with a certificate error that says nothing
// about the missing bundle. The path is made absolute because the variable is
// read by every node process the inspector starts, not all of them in this
// working directory.
func caCertPath(value string) (string, error) {
	if strings.ContainsAny(value, "\x00\n\r") {
		return "", errors.New("--ca-cert must not contain control characters")
	}
	path, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("--ca-cert is not a usable path%s: %w", argumentWithheld, pathErrorReason(err))
	}
	// The kind of file is settled before it is opened: opening a named pipe
	// blocks until a writer appears, so a value naming one would hang the
	// command instead of being refused.
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("--ca-cert must name a readable certificate file%s: %w", argumentWithheld, pathErrorReason(err))
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("--ca-cert must name a regular file" + argumentWithheld)
	}
	// Opening is the readability check, not stating: node reads the bundle, so
	// a file this user may not read is as useless as an absent one and must be
	// refused with the same clear message.
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("--ca-cert must name a readable certificate file%s: %w", argumentWithheld, pathErrorReason(err))
	}
	defer f.Close()
	return path, nil
}

// pathErrorReason returns the reason a file operation failed without the path
// it failed on. The operating system's errors quote the path, which here is
// the value the operator passed.
func pathErrorReason(err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return linkErr.Err
	}
	var syscallErr *os.SyscallError
	if errors.As(err, &syscallErr) {
		return syscallErr.Err
	}
	return errors.New("the path cannot be used")
}

// validateEnvValue rejects a flag value that cannot safely become an
// environment variable for the child process.
func validateEnvValue(flag, value string) error {
	if value == "" {
		return nil
	}
	if strings.ContainsAny(value, "\x00\n\r=") {
		return fmt.Errorf("%s must not contain control characters or '='", flag)
	}
	return nil
}

// isVersionSpec reports whether v is a plausible npm version or dist-tag, so an
// arbitrary string cannot be appended to the package argument.
func isVersionSpec(v string) bool {
	if len(v) > 64 {
		return false
	}
	for _, r := range v {
		switch {
		case r >= '0' && r <= '9',
			r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r == '.' || r == '-' || r == '+':
		default:
			return false
		}
	}
	return v != ""
}

// versionMajor reads the major number of an explicit npm version. It reports
// false for a dist-tag such as "latest", which names no version until npm
// resolves it.
func versionMajor(v string) (int, bool) {
	digits, _, _ := strings.Cut(v, ".")
	major, err := strconv.Atoi(digits)
	if err != nil || major < 0 {
		return 0, false
	}
	return major, true
}

// writeInspectorConfig writes the inspector configuration for one server to a
// private temporary file and returns its path.
func writeInspectorConfig(name string, config map[string]any) (string, error) {
	// HTML escaping is off so a url keeps its "&" and "/" characters as written:
	// the file is read by the inspector and by whoever is debugging the launch.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "    ")
	if err := enc.Encode(map[string]any{
		"mcpServers": map[string]any{name: config},
	}); err != nil {
		return "", fmt.Errorf("mcp: encode inspector configuration: %w", err)
	}
	body := buf.Bytes()

	// CreateTemp opens the file 0600: the configuration may embed a url with an
	// access token, so it stays readable by this user only.
	f, err := os.CreateTemp("", "mcp-inspector-*.json")
	if err != nil {
		return "", fmt.Errorf("mcp: create the inspector configuration file: %w", err)
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("mcp: write the inspector configuration file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("mcp: write the inspector configuration file: %w", err)
	}
	return f.Name(), nil
}

// projectBinary locates the command binary the inspector should launch for a
// stdio session: the project's ./vel binary when it is present, otherwise the
// running executable, which serves the same commands.
func projectBinary() (string, error) {
	if path, err := filepath.Abs("vel"); err == nil {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return path, nil
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("mcp: locate the project binary, pass --command: %w", err)
	}
	return exe, nil
}

// promptForValue asks the operator for a single value on the terminal.
func promptForValue(question string) string {
	return prism.Text(question)
}

// runInspectorProcess is the production runner: it starts npx with the current
// environment plus the launch overlay and streams the inspector's output to the
// terminal until it exits or the context is cancelled.
//
// Cancellation is how an inspector session normally ends: the operator
// interrupts the command, which cancels the context. That is a deliberate stop,
// so the run reports success rather than a failure, and the inspector is asked
// to stop with an interrupt of its own instead of being killed, giving it the
// chance to shut down the node children serving the inspector UI and proxy
// before they are orphaned on their ports.
//
// The inspector is started the way a shell starts a command it is asked to
// run: in the command's own process group, with the command's standard input.
// A terminal therefore treats the two as one job. Its interrupt, quit and
// suspend keys reach the inspector and everything the inspector starts
// directly, a suspended job is resumed whole, and npx can ask on the terminal
// before it installs the pinned package for the first time.
//
// npx is only the first of the processes the launch starts, and the ones
// holding the ports are started by it. A stop that reaches the command alone
// (a signal sent to it rather than typed at the terminal) is passed on to
// all of them, and once npx has exited, by itself or killed after the grace
// period, whatever the run started that is still alive is ended before the
// run returns, so nothing started by the command outlives it. A signal typed
// at the terminal can end npx before the command has been handed its own
// copy; the run waits a moment for that copy (inspectorSignalArrival) rather
// than report the stop as a failed inspector. Sharing a
// group means the group cannot be signalled as a whole; startedProcesses
// describes how the run's processes are picked out of it, and placeInspector
// what is done on a platform where the group cannot be listed at all.
func runInspectorProcess(ctx context.Context, l inspectorLaunch) error {
	cmd := exec.CommandContext(ctx, l.Name, l.Args...)
	cmd.Env = inspectorEnviron(os.Environ(), l.Env)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	before := placeInspector(cmd)
	cmd.Cancel = func() error {
		var sig os.Signal = os.Interrupt
		var stop stoppedBySignal
		if errors.As(context.Cause(ctx), &stop) {
			sig = stop.sig
		}
		return interruptInspector(cmd, before, relayedSignal(sig))
	}
	// An inspector that ignores the interrupt is killed once the grace period
	// is over, so the command cannot hang on to the terminal.
	cmd.WaitDelay = inspectorShutdownGrace

	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("mcp: start the MCP Inspector: %w", err)
	}
	err := cmd.Wait()
	endStartedProcesses(cmd, before)
	if err != nil {
		if ctx.Err() == nil && endedByStopSignal(err) {
			// The signal that ended the inspector was most likely sent to the
			// whole job, and the command's copy is then about to cancel the
			// run. Only when none arrives was the inspector signalled alone,
			// which is a failure of the inspector like any other.
			arrival := time.NewTimer(inspectorSignalArrival)
			select {
			case <-ctx.Done():
			case <-arrival.C:
			}
			arrival.Stop()
		}
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("mcp: the MCP Inspector exited: %w", err)
	}
	return nil
}

// withheldVariables and withheldPrefixes name the inherited settings the
// inspector process is not handed, compared case-folded (see
// inheritedVariableWithheld). They are grouped by the program that reads them,
// and each group is withheld whole rather than one name at a time, because
// every member of it can change what the launch verified and announced:
//
//   - node_*: the node runtime's own settings. NODE_OPTIONS injects any flag
//     (a TLS protocol floor, an insecure parser, a preloaded module),
//     NODE_TLS_REJECT_UNAUTHORIZED switches certificate verification off,
//     NODE_EXTRA_CA_CERTS adds a trust anchor the guidance never named,
//     NODE_PATH and NODE_COMPILE_CACHE load code from elsewhere.
//   - npm_config_*: npm's settings, which npx fetches the inspector under.
//     They choose the registry, whether its certificate is verified
//     (strict-ssl), which trust store is used (ca, cafile), and which config
//     file is read next (userconfig, globalconfig), so no one of them can be
//     allowed through without the others following.
//   - OPENSSL_CONF, SSL_CERT_FILE and SSL_CERT_DIR: the OpenSSL stack node
//     links, which can lower the protocol floor or swap the trust store
//     underneath every node setting above.
//   - the inspector's own switches: DANGEROUSLY_OMIT_AUTH disables the proxy
//     session token, MCP_PROXY_AUTH_TOKEN fixes it to a known value,
//     ALLOWED_ORIGINS widens the origin check that keeps other sites from
//     driving the proxy, HOST binds the UI and proxy beyond the loopback
//     interface, and MCP_PROXY_FULL_ADDRESS tells the UI to drive a proxy
//     other than the one this command started.
//
// The values the command sets itself (HOST, CLIENT_PORT, NODE_EXTRA_CA_CERTS)
// travel in the launch overlay, which is applied after the inherited
// environment has been filtered.
var (
	withheldPrefixes  = []string{"node_", "npm_config_"}
	withheldVariables = []string{
		"openssl_conf", "ssl_cert_file", "ssl_cert_dir",
		"dangerously_omit_auth", "mcp_proxy_auth_token", "allowed_origins", "host", "mcp_proxy_full_address",
	}
)

// inheritedVariableWithheld reports whether an inherited variable is kept from
// the inspector process. The name is compared case-folded: Windows resolves
// variables without regard to case, and npm reads its npm_config_ settings
// that way on every platform, so a setting blocked in one spelling would
// otherwise reach the child in another.
func inheritedVariableWithheld(name string) bool {
	folded := str.Lower(name)
	return str.StartsWith(folded, withheldPrefixes...) || slices.Contains(withheldVariables, folded)
}

// inspectorEnviron builds the environment of the inspector process: the one
// this command was started with, without the settings inheritedVariableWithheld
// names, plus the launch overlay. A variable the overlay sets replaces every
// inherited spelling of it, so the child reads one value for it and it is the
// one the command decided on.
//
// Node applies its TLS settings to every connection the process makes, so a
// value exported in the operator's shell would follow the session to the
// inspected server, to the authorization server of an OAuth-protected session,
// and to the registry; npm applies its own to the fetch of the inspector
// itself. Inheriting any of them would leave the launch without the
// verification it announces, for reasons outside the launch, so they are
// dropped here; a development certificate is trusted by naming its issuing CA
// with --ca-cert, which adds an anchor rather than removing verification.
func inspectorEnviron(parent []string, overlay map[string]string) []string {
	overlaid := make(map[string]struct{}, len(overlay))
	for name := range overlay {
		overlaid[str.Lower(name)] = struct{}{}
	}

	env := make([]string, 0, len(parent)+len(overlay))
	for _, entry := range parent {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			env = append(env, entry)
			continue
		}
		if inheritedVariableWithheld(name) {
			continue
		}
		if _, replaced := overlaid[str.Lower(name)]; replaced {
			continue
		}
		env = append(env, entry)
	}
	return append(env, envPairs(overlay)...)
}

// envPairs renders an environment overlay as KEY=VALUE entries in a stable
// order.
func envPairs(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, k+"="+env[k])
	}
	return pairs
}
