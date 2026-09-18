package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/velocitykode/prism"
	velapp "github.com/velocitykode/velocity/app"

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

	// inspectorShutdownGrace is how long the inspector is given to shut its own
	// node children down after an interrupt before it is killed outright.
	inspectorShutdownGrace = 5 * time.Second

	// startCommandName is the command the inspector launches for a stdio
	// session; it is served by startCommand in this package.
	startCommandName = "mcp:start"
)

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
// inspector until it exits or the process is interrupted.
func (c inspectorCommand) Handle(s *velapp.Services, args []string) error {
	if c.srv == nil {
		return errors.New("mcp: inspector command built without a server")
	}

	opts, err := parseInspectorArgs(args)
	if err != nil {
		return err
	}

	// Announced before anything else happens, so the operator knows which
	// server they are answering route parameter questions for.
	if err := c.writeHeader(); err != nil {
		return err
	}

	config, guidance, env, err := c.transportConfig(opts)
	if err != nil {
		return err
	}

	configPath, err := writeInspectorConfig(c.srv.Name(), config)
	if err != nil {
		return err
	}
	// The inspector reads the file at startup; it is ours to clean up, and it
	// may carry a url with credentials in it, so it does not outlive the run.
	defer func() { _ = os.Remove(configPath) }()

	guidance = append(guidance,
		[2]string{"Protocol Era", inspectorProtocolEra},
		[2]string{"Config", filepath.ToSlash(configPath)},
	)
	if err := c.writeGuidance(guidance); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	run := c.run
	if run == nil {
		run = runInspectorProcess
	}
	return run(ctx, inspectorLaunch{
		Name:       "npx",
		Args:       []string{opts.packageSpec(), "--config", configPath},
		Env:        env,
		ConfigPath: configPath,
	})
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
		{"URL", serverURL},
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

	parsed, err := url.Parse(filled)
	if err != nil {
		return nil, fmt.Errorf("mcp: --url is not a valid url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("mcp: --url must be an absolute http or https url, got %q", filled)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("mcp: --url is missing a host: %q", filled)
	}
	return parsed, nil
}

// fillRouteParameters replaces each {name} placeholder of a route path with a
// value supplied by the operator. A trailing ":constraint" inside the braces is
// part of the route declaration, not of the name, and the constraint is a regex
// that may carry braces of its own, as in "{id:[0-9]{2}}". Values are
// percent-encoded so an answer containing a slash or a space cannot reshape the
// url.
func (c inspectorCommand) fillRouteParameters(raw string) (string, error) {
	ask := c.ask
	if ask == nil {
		ask = promptForValue
	}

	var b strings.Builder
	rest := raw
	for {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			b.WriteString(rest)
			return b.String(), nil
		}
		end := routeParameterEnd(rest, open)
		if end < 0 {
			return "", fmt.Errorf("mcp: unterminated route parameter in --url: %q", raw)
		}

		name := rest[open+1 : end]
		if i := strings.IndexByte(name, ':'); i >= 0 {
			name = name[:i]
		}
		if strings.TrimSpace(name) == "" {
			return "", fmt.Errorf("mcp: unnamed route parameter in --url: %q", raw)
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
			return inspectorOptions{}, fmt.Errorf("unknown flag %q", arg)
		case !ok:
			return inspectorOptions{}, fmt.Errorf("unexpected argument %q (usage: vel run mcp:inspector [--url URL] [--host HOST] [--port PORT] [--ca-cert FILE])", arg)
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
			return inspectorOptions{}, fmt.Errorf("--port must be a number between 1 and 65535, got %q", opts.port)
		}
	}
	if opts.version != "" {
		if !isVersionSpec(opts.version) {
			return inspectorOptions{}, fmt.Errorf("--inspector-version must be an npm version or tag, got %q", opts.version)
		}
		// A dist-tag cannot be resolved without reaching npm, so only an
		// explicit version is checked against the launch contract.
		if major, ok := versionMajor(opts.version); ok && major < inspectorMinimumMajor {
			return inspectorOptions{}, fmt.Errorf("--inspector-version must be %d.0.0 or newer, got %q: older inspectors do not understand the generated configuration file", inspectorMinimumMajor, opts.version)
		}
	}
	if opts.command != "" && strings.ContainsAny(opts.command, "\x00\n\r") {
		return inspectorOptions{}, errors.New("--command must not contain control characters")
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
		return "", fmt.Errorf("--ca-cert is not a usable path %q: %w", value, err)
	}
	// The kind of file is settled before it is opened: opening a named pipe
	// blocks until a writer appears, so a value naming one would hang the
	// command instead of being refused.
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("--ca-cert must name a readable certificate file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("--ca-cert must name a regular file, got %q", value)
	}
	// Opening is the readability check, not stating: node reads the bundle, so
	// a file this user may not read is as useless as an absent one and must be
	// refused with the same clear message.
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("--ca-cert must name a readable certificate file: %w", err)
	}
	defer f.Close()
	return path, nil
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
func runInspectorProcess(ctx context.Context, l inspectorLaunch) error {
	cmd := exec.CommandContext(ctx, l.Name, l.Args...)
	cmd.Env = inspectorEnviron(os.Environ(), l.Env)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	// An inspector that ignores the interrupt is killed once the grace period
	// is over, so the command cannot hang on to the terminal.
	cmd.WaitDelay = inspectorShutdownGrace

	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("mcp: start the MCP Inspector: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("mcp: the MCP Inspector exited: %w", err)
	}
	return nil
}

// tlsRejectUnauthorizedVar is the node setting that switches certificate
// verification off for every connection the process makes. This command never
// sets it, and drops it from the environment it inherits.
const tlsRejectUnauthorizedVar = "NODE_TLS_REJECT_UNAUTHORIZED"

// inspectorEnviron builds the environment of the inspector process: the one
// this command was started with, without the certificate exception, plus the
// launch overlay.
//
// Node applies tlsRejectUnauthorizedVar to every connection it makes, so a
// value exported in the operator's shell would follow the session to the
// inspected server, to the authorization server of an OAuth-protected session,
// and to the registry. Inheriting it would leave the launch without
// verification for reasons outside the launch, so it is dropped here; a
// development certificate is trusted by naming its issuing CA with --ca-cert,
// which adds an anchor rather than removing verification.
func inspectorEnviron(parent []string, overlay map[string]string) []string {
	env := make([]string, 0, len(parent)+len(overlay))
	for _, entry := range parent {
		if name, _, ok := strings.Cut(entry, "="); ok && name == tlsRejectUnauthorizedVar {
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
