package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/velocitykode/velocity-mcp/server"
)

// inspectorConfig is the configuration file the inspector is handed, decoded
// back from disk so the tests assert the bytes the inspector would read.
type inspectorConfig struct {
	Servers map[string]struct {
		Type        string   `json:"type"`
		Command     string   `json:"command"`
		Args        []string `json:"args"`
		URL         string   `json:"url"`
		ProtocolEra string   `json:"protocolEra"`
	} `json:"mcpServers"`
}

// runRecorder captures what the command would have launched, including the
// configuration file as it exists at launch time, and never starts a process.
type runRecorder struct {
	mu       sync.Mutex
	calls    int
	launch   inspectorLaunch
	config   inspectorConfig
	rawBytes []byte
	mode     fs.FileMode
	err      error
}

func (r *runRecorder) run(ctx context.Context, l inspectorLaunch) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.launch = l
	if body, err := os.ReadFile(l.ConfigPath); err == nil {
		r.rawBytes = body
		_ = json.Unmarshal(body, &r.config)
	}
	if info, err := os.Stat(l.ConfigPath); err == nil {
		r.mode = info.Mode().Perm()
	}
	return r.err
}

// newInspector builds the command with every external effect injected: no npx,
// no terminal prompt, no filesystem probing for the project binary.
func newInspector(t *testing.T, rec *runRecorder, answers ...string) (inspectorCommand, *bytes.Buffer, *[]string) {
	t.Helper()
	out := &bytes.Buffer{}
	var asked []string
	i := 0
	cmd := inspectorCommand{
		srv: inventoryServer(),
		out: out,
		run: rec.run,
		ask: func(question string) string {
			asked = append(asked, question)
			if i < len(answers) {
				answer := answers[i]
				i++
				return answer
			}
			return ""
		},
		binary: func() (string, error) { return "/projects/demo/vel", nil },
	}
	return cmd, out, &asked
}

func TestInspectorRegisteredWithTheServerCommands(t *testing.T) {
	var found bool
	for _, c := range ServerCommands(inventoryServer()) {
		if c.Name() == "mcp:inspector" {
			found = true
			if c.Description() == "" {
				t.Fatal("mcp:inspector has no description")
			}
		}
	}
	if !found {
		t.Fatal("mcp:inspector not registered by ServerCommands")
	}
}

func TestInspectorLaunchesStdioByDefault(t *testing.T) {
	rec := &runRecorder{}
	cmd, out, _ := newInspector(t, rec)

	if err := cmd.Handle(nil, nil); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if rec.calls != 1 {
		t.Fatalf("runner called %d times, want 1", rec.calls)
	}

	if rec.launch.Name != "npx" {
		t.Fatalf("executable = %q, want npx", rec.launch.Name)
	}
	// The configuration file is handed over on its own. An inspector before
	// major 2 refuses --config unless --server also names an entry in it, so the
	// argument vector and the pinned version have to agree.
	wantArgs := []string{rec.launch.Args[0], "--config", rec.launch.ConfigPath}
	if !reflect.DeepEqual(rec.launch.Args, wantArgs) {
		t.Fatalf("args = %q, want the package spec followed by --config %q", rec.launch.Args, rec.launch.ConfigPath)
	}
	assertInspectorSpec(t, rec.launch.Args[0])
	if len(rec.launch.Env) != 0 {
		t.Fatalf("env = %v, want empty without --host/--port", rec.launch.Env)
	}

	entry, ok := rec.config.Servers["demo"]
	if !ok {
		t.Fatalf("configuration is not keyed by the server name: %s", rec.rawBytes)
	}
	if entry.Type != "stdio" || entry.ProtocolEra != "auto" {
		t.Fatalf("entry = %+v, want a stdio entry negotiating the protocol", entry)
	}
	if entry.Command != "/projects/demo/vel" {
		t.Fatalf("command = %q", entry.Command)
	}
	if !reflect.DeepEqual(entry.Args, []string{"run", "mcp:start"}) {
		t.Fatalf("args = %q, want the mcp:start command", entry.Args)
	}
	if entry.URL != "" {
		t.Fatalf("stdio entry must not carry a url, got %q", entry.URL)
	}
	if rec.mode != 0o600 {
		t.Fatalf("configuration file mode = %04o, want 0600", rec.mode)
	}

	for _, want := range []string{
		"Starting the MCP Inspector for server [demo]\n",
		"Transport Type => STDIO\n",
		"Command => /projects/demo/vel\n",
		"Arguments => run mcp:start\n",
		"Protocol Era => auto\n",
		"Config => " + rec.launch.ConfigPath + "\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("guidance missing %q in:\n%s", want, out.String())
		}
	}

	// The configuration may carry credentials, so it does not outlive the run.
	if _, err := os.Stat(rec.launch.ConfigPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("configuration file still present after the run: %v", err)
	}
}

func TestInspectorCommandOverridesTheStdioBinary(t *testing.T) {
	rec := &runRecorder{}
	cmd, out, _ := newInspector(t, rec)

	if err := cmd.Handle(nil, []string{"--command", "/usr/local/bin/demo"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := rec.config.Servers["demo"].Command; got != "/usr/local/bin/demo" {
		t.Fatalf("command = %q, want the --command override", got)
	}
	if !strings.Contains(out.String(), "Command => /usr/local/bin/demo\n") {
		t.Fatalf("guidance did not report the override:\n%s", out.String())
	}
}

func TestInspectorWebTransport(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantURL string
		// wantHint is the certificate guidance the run must print, empty for a
		// url that carries no certificate.
		wantHint string
	}{
		{"http carries no certificate guidance", "http://localhost:8080/mcp", "http://localhost:8080/mcp", ""},
		{"https on localhost keeps verification on", "https://localhost:8443/mcp", "https://localhost:8443/mcp", localCertHint},
		{"https on a development domain keeps verification on", "https://demo.test/mcp", "https://demo.test/mcp", localCertHint},
		{"https on a remote host keeps verification on", "https://api.example.com/mcp", "https://api.example.com/mcp", remoteCertHint},
		{"https on an mdns host keeps verification on", "https://mac.local/mcp", "https://mac.local/mcp", remoteCertHint},
		{"query string survives", "http://localhost:8080/mcp?tenant=acme", "http://localhost:8080/mcp?tenant=acme", ""},
		// Several parameters mean an "&" in the url: it must reach the
		// inspector as one ampersand, neither escaped away nor doubled.
		{"multi parameter query string survives", "http://localhost:8080/mcp?tenant=acme&region=eu", "http://localhost:8080/mcp?tenant=acme&region=eu", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &runRecorder{}
			cmd, out, _ := newInspector(t, rec)

			if err := cmd.Handle(nil, []string{"--url", tc.url}); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			entry := rec.config.Servers["demo"]
			if entry.Type != "http" || entry.ProtocolEra != "auto" {
				t.Fatalf("entry = %+v, want an http entry negotiating the protocol", entry)
			}
			if entry.URL != tc.wantURL {
				t.Fatalf("url = %q, want %q", entry.URL, tc.wantURL)
			}
			// The file itself carries the url as written, so an operator
			// reading it sees the address the inspector will dial.
			if !strings.Contains(string(rec.rawBytes), tc.wantURL) {
				t.Fatalf("configuration file does not carry the url verbatim:\n%s", rec.rawBytes)
			}
			if entry.Command != "" || len(entry.Args) != 0 {
				t.Fatalf("http entry must not carry a command: %+v", entry)
			}

			assertVerifiesCertificates(t, rec.launch.Env)

			wantGuidance := []string{"Transport Type => Streamable HTTP\n", "URL => " + tc.wantURL + "\n"}
			if tc.wantHint != "" {
				wantGuidance = append(wantGuidance, "Certificates => "+tc.wantHint+"\n")
			}
			for _, want := range wantGuidance {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("guidance missing %q in:\n%s", want, out.String())
				}
			}
			if tc.wantHint == "" && strings.Contains(out.String(), "Certificates =>") {
				t.Fatalf("certificate guidance printed for a url without one:\n%s", out.String())
			}
		})
	}
}

// The certificate guidance a run prints, written out here rather than read from
// the command, so a change of wording is a deliberate change of the contract.
const (
	localCertHint  = "the server certificate must chain to a trusted CA; for a development certificate pass --ca-cert with its issuing CA"
	remoteCertHint = "the server certificate must chain to a trusted CA"
)

// assertVerifiesCertificates fails when a launch would leave the inspector
// process without certificate verification. Node applies these settings to every
// connection it makes, including the requests an OAuth-protected session sends
// to the authorization server, so an exception meant for the inspected server
// would follow the session to every other host it talks to.
func assertVerifiesCertificates(t *testing.T, env map[string]string) {
	t.Helper()
	if v, ok := env["NODE_TLS_REJECT_UNAUTHORIZED"]; ok {
		t.Fatalf("NODE_TLS_REJECT_UNAUTHORIZED = %q: the launch must never switch certificate verification off", v)
	}
	if opts := env["NODE_OPTIONS"]; strings.Contains(opts, "tls-reject") || strings.Contains(opts, "insecure") {
		t.Fatalf("NODE_OPTIONS = %q switches certificate verification off", opts)
	}
}

// writeCACert writes a certificate file for a run to trust. Only its path ever
// reaches the command.
func writeCACert(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dev-ca.pem")
	const pem = "-----BEGIN CERTIFICATE-----\nZGV2ZWxvcG1lbnQ=\n-----END CERTIFICATE-----\n"
	if err := os.WriteFile(path, []byte(pem), 0o600); err != nil {
		t.Fatalf("write the certificate: %v", err)
	}
	return path
}

// A development certificate is accepted by trusting the authority that issued
// it, which adds an anchor to the ones node already carries. Verification stays
// on for the inspected server and for every other host the inspector reaches.
func TestInspectorTrustsAnExtraCertificateAuthority(t *testing.T) {
	ca := writeCACert(t)

	cases := []struct {
		name string
		args []string
	}{
		{"https on a local host", []string{"--url", "https://localhost:8443/mcp", "--ca-cert", ca}},
		{"https on a remote host", []string{"--url", "https://api.example.com/mcp", "--ca-cert=" + ca}},
		{"stdio, for the authorization server", []string{"--ca-cert", ca}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &runRecorder{}
			cmd, out, _ := newInspector(t, rec)

			if err := cmd.Handle(nil, tc.args); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			assertVerifiesCertificates(t, rec.launch.Env)
			if got := rec.launch.Env["NODE_EXTRA_CA_CERTS"]; got != ca {
				t.Fatalf("NODE_EXTRA_CA_CERTS = %q, want the certificate path %q", got, ca)
			}
			want := "Certificates => verified against the system anchors and " + filepath.ToSlash(ca) + "\n"
			if !strings.Contains(out.String(), want) {
				t.Fatalf("guidance missing %q in:\n%s", want, out.String())
			}
		})
	}
}

// A relative --ca-cert is resolved before it is handed over: the variable is
// read by node processes the inspector starts elsewhere.
func TestInspectorResolvesTheCACertificatePath(t *testing.T) {
	ca := writeCACert(t)
	dir, base := filepath.Split(ca)
	t.Chdir(filepath.Clean(dir))

	rec := &runRecorder{}
	cmd, _, _ := newInspector(t, rec)

	if err := cmd.Handle(nil, []string{"--ca-cert", base}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := rec.launch.Env["NODE_EXTRA_CA_CERTS"]
	if !filepath.IsAbs(got) {
		t.Fatalf("NODE_EXTRA_CA_CERTS = %q, want an absolute path", got)
	}
	// The name alone would also match a path resolved against the wrong
	// directory, so the two must be the same file on disk.
	want, err := os.Stat(ca)
	if err != nil {
		t.Fatalf("stat the certificate: %v", err)
	}
	resolved, err := os.Stat(got)
	if err != nil {
		t.Fatalf("NODE_EXTRA_CA_CERTS = %q, which does not exist: %v", got, err)
	}
	if !os.SameFile(want, resolved) {
		t.Fatalf("NODE_EXTRA_CA_CERTS = %q, want the certificate passed as %q", got, base)
	}
}

// unreadableFile writes a certificate file whose permissions deny this process,
// and reports whether the denial took effect: a superuser, or a filesystem that
// ignores the mode, reads it anyway and has no unreadable file to offer.
func unreadableFile(t *testing.T) (string, bool) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "unreadable.pem")
	if err := os.WriteFile(path, []byte("-----BEGIN CERTIFICATE-----\n"), 0o000); err != nil {
		t.Fatalf("write the certificate: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		return path, true
	}
	f.Close()
	return path, false
}

// A certificate path node could not read would leave the operator with a
// verification failure that says nothing about the file, so the run stops
// before anything is launched. A file that exists but is closed to this user is
// one of those paths: node ignores it exactly as it ignores an absent one.
func TestInspectorRejectsAnUnusableCACertificate(t *testing.T) {
	dir := t.TempDir()

	type testCase struct {
		name  string
		value string
		want  string
	}
	cases := []testCase{
		{"no such file", filepath.Join(dir, "absent.pem"), "readable certificate file"},
		{"a directory", dir, "regular file"},
		{"nul byte", filepath.Join(dir, "ca.pem") + "\x00", "control characters"},
		{"newline", filepath.Join(dir, "ca.pem") + "\nNODE_TLS_REJECT_UNAUTHORIZED=0", "control characters"},
	}
	if path, denied := unreadableFile(t); denied {
		cases = append(cases, testCase{"a file this user cannot read", path, "readable certificate file"})
	} else {
		t.Log("this user reads a file with no permission bits set, so that case is not exercised here")
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &runRecorder{}
			cmd, _, _ := newInspector(t, rec)

			err := cmd.Handle(nil, []string{"--url", "https://localhost:8443/mcp", "--ca-cert", tc.value})
			if err == nil {
				t.Fatal("an unusable certificate path was accepted, want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
			if rec.calls != 0 {
				t.Fatal("the inspector was launched with an unusable certificate path")
			}
		})
	}
}

func TestIsLocalHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"localhost", true},
		{"LOCALHOST", true},
		{"localhost.", true},
		{"127.0.0.1", true},
		{"127.1.2.3", true},
		{"::1", true},
		{"demo.test", true},
		{"demo.localhost", true},
		// ".local" is the mDNS suffix of every machine on the network, not of
		// this one, so it does not relax certificate verification.
		{"mac.local", false},
		{"api.example.com", false},
		{"", false},
		{"127.example.com", false},
		{"notlocalhost.example", false},
		{"testing.example", false},
		{"8.8.8.8", false},
	}
	for _, tc := range cases {
		if got := isLocalHost(tc.host); got != tc.want {
			t.Fatalf("isLocalHost(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

func TestInspectorPromptsForRouteParameters(t *testing.T) {
	rec := &runRecorder{}
	cmd, out, asked := newInspector(t, rec, "4f8a1c2e", "acme corp/eu")

	// The operator has to know which server they are answering for, so the run
	// is announced before the first question.
	var printedWhenAsked string
	prompt := cmd.ask
	cmd.ask = func(question string) string {
		if printedWhenAsked == "" {
			printedWhenAsked = out.String()
		}
		return prompt(question)
	}

	err := cmd.Handle(nil, []string{"--url", "http://localhost:8080/mcp/{organisation:uuid}/{team}"})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	wantAsked := []string{
		"What is the value for the [organisation] route parameter?",
		"What is the value for the [team] route parameter?",
	}
	if !reflect.DeepEqual(*asked, wantAsked) {
		t.Fatalf("questions = %q, want %q", *asked, wantAsked)
	}
	if !strings.Contains(printedWhenAsked, "Starting the MCP Inspector for server [demo]") {
		t.Fatalf("output before the first question = %q, want the run announced first", printedWhenAsked)
	}
	// The answer is percent-encoded, so a value containing a separator cannot
	// reshape the url into a different route.
	if got := rec.config.Servers["demo"].URL; got != "http://localhost:8080/mcp/4f8a1c2e/acme%20corp%2Feu" {
		t.Fatalf("url = %q", got)
	}
}

// A route parameter may carry a regex constraint, and a regex carries braces of
// its own: a quantifier nests a pair inside the placeholder, a character class
// can hold one that is no delimiter at all, and an escape can hide one. The
// placeholder ends at the brace that closes it, never at the first one seen.
func TestInspectorFillsRouteParametersWithRegexConstraints(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		answers []string
		asked   []string
		want    string
	}{
		{
			name:    "quantifier",
			url:     "http://localhost:8080/mcp/{id:[0-9]{2}}",
			answers: []string{"42"},
			asked:   []string{"What is the value for the [id] route parameter?"},
			want:    "http://localhost:8080/mcp/42",
		},
		{
			name:    "bounded quantifier",
			url:     "http://localhost:8080/mcp/{code:[a-z]{1,3}}/tools",
			answers: []string{"abc"},
			asked:   []string{"What is the value for the [code] route parameter?"},
			want:    "http://localhost:8080/mcp/abc/tools",
		},
		{
			name:    "braces inside a character class",
			url:     "http://localhost:8080/mcp/{id:[0-9{}]+}",
			answers: []string{"42"},
			asked:   []string{"What is the value for the [id] route parameter?"},
			want:    "http://localhost:8080/mcp/42",
		},
		{
			name:    "escaped braces",
			url:     `http://localhost:8080/mcp/{id:\{[0-9]+\}}`,
			answers: []string{"42"},
			asked:   []string{"What is the value for the [id] route parameter?"},
			want:    "http://localhost:8080/mcp/42",
		},
		{
			name:    "constrained parameter followed by a plain one",
			url:     "http://localhost:8080/mcp/{organisation:[a-z]{2}}/{team}",
			answers: []string{"eu", "red"},
			asked: []string{
				"What is the value for the [organisation] route parameter?",
				"What is the value for the [team] route parameter?",
			},
			want: "http://localhost:8080/mcp/eu/red",
		},
		{
			name:    "wildcard constraint",
			url:     "http://localhost:8080/mcp/{path:.*}",
			answers: []string{"a/b"},
			asked:   []string{"What is the value for the [path] route parameter?"},
			want:    "http://localhost:8080/mcp/a%2Fb",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &runRecorder{}
			cmd, _, asked := newInspector(t, rec, tc.answers...)

			if err := cmd.Handle(nil, []string{"--url", tc.url}); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if !reflect.DeepEqual(*asked, tc.asked) {
				t.Fatalf("questions = %q, want %q", *asked, tc.asked)
			}
			if got := rec.config.Servers["demo"].URL; got != tc.want {
				t.Fatalf("url = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInspectorRejectsBlankRouteParameters(t *testing.T) {
	cases := []struct {
		name   string
		answer string
	}{
		{"empty answer", ""},
		{"whitespace only", "   "},
		{"tab and newline", "\t\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &runRecorder{}
			cmd, _, _ := newInspector(t, rec, tc.answer)

			err := cmd.Handle(nil, []string{"--url", "http://localhost/mcp/{organisation}"})
			if err == nil {
				t.Fatal("blank route parameter accepted, want an error")
			}
			if !strings.Contains(err.Error(), "every route parameter needs a value to inspect this server") {
				t.Fatalf("error = %v", err)
			}
			if rec.calls != 0 {
				t.Fatal("the inspector was launched despite the missing parameter")
			}
		})
	}
}

func TestInspectorRejectsBadURLs(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"unterminated parameter", "http://localhost/mcp/{organisation", "unterminated route parameter"},
		{"unterminated constrained parameter", "http://localhost/mcp/{id:[0-9]{2}", "unterminated route parameter"},
		{"parameter spanning two segments", "http://localhost/mcp/{organisation/team}", "unterminated route parameter"},
		{"unnamed parameter", "http://localhost/mcp/{}", "unnamed route parameter"},
		{"relative path", "/mcp", "absolute http or https url"},
		{"wrong scheme", "ftp://localhost/mcp", "absolute http or https url"},
		{"file scheme", "file:///etc/passwd", "absolute http or https url"},
		{"no host", "http:///mcp", "missing a host"},
		{"control character", "http://localhost/\x7f\x00mcp", "not a valid url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &runRecorder{}
			cmd, _, _ := newInspector(t, rec, "value")

			err := cmd.Handle(nil, []string{"--url", tc.url})
			if err == nil {
				t.Fatalf("url %q accepted, want an error", tc.url)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
			if rec.calls != 0 {
				t.Fatal("the inspector was launched for an invalid url")
			}
		})
	}
}

func TestInspectorPassesHostAndPortThroughTheEnvironment(t *testing.T) {
	rec := &runRecorder{}
	cmd, _, _ := newInspector(t, rec)

	if err := cmd.Handle(nil, []string{"--host", "0.0.0.0", "--port=6274"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	want := map[string]string{"HOST": "0.0.0.0", "CLIENT_PORT": "6274"}
	if !reflect.DeepEqual(rec.launch.Env, want) {
		t.Fatalf("env = %v, want %v", rec.launch.Env, want)
	}
}

func TestInspectorVersionOverrideIsLaunched(t *testing.T) {
	rec := &runRecorder{}
	cmd, _, _ := newInspector(t, rec)

	if err := cmd.Handle(nil, []string{"--inspector-version", "2.9.1"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := rec.launch.Args[0]; got != "@modelcontextprotocol/inspector@2.9.1" {
		t.Fatalf("package = %q, want the requested version", got)
	}
}

// assertInspectorSpec checks a launched package spec against the inspector CLI
// this command is written for, rather than against whatever version the code
// happens to name. The launch hands over a configuration file alone, with a
// server entry carrying "type", "url" and "protocolEra": an inspector 0.x or
// 1.x rejects --config without --server and reads only command/args/env from
// the entry, so anything below major 2 cannot run it.
func assertInspectorSpec(t *testing.T, spec string) {
	t.Helper()
	const prefix = "@modelcontextprotocol/inspector@"
	if !strings.HasPrefix(spec, prefix) {
		t.Fatalf("package spec = %q, want it to pin %s", spec, prefix)
	}
	version := strings.TrimPrefix(spec, prefix)
	major, ok := versionMajor(version)
	if !ok {
		t.Fatalf("package spec = %q, want an explicit pinned version", spec)
	}
	if major < 2 {
		t.Fatalf("package spec = %q: inspector %d.x cannot run a --config launch with a typed server entry", spec, major)
	}
}

// The configuration names the protocol era the inspector negotiates in. The
// "modern" era makes it require the discovery method introduced in protocol
// version 2026-07-28 and refuse to fall back, so it may only be selected once
// this server serves that version; until then the inspector has to be left free
// to negotiate.
func TestInspectorProtocolEraMatchesTheServerProtocol(t *testing.T) {
	const discoveryProtocolVersion = "2026-07-28"

	switch inspectorProtocolEra {
	case "auto", "legacy":
	case "modern":
		if server.LatestProtocolVersion < discoveryProtocolVersion {
			t.Fatalf("protocol era %q requires the server to serve %s, but the newest version it serves is %s",
				inspectorProtocolEra, discoveryProtocolVersion, server.LatestProtocolVersion)
		}
	default:
		t.Fatalf("protocol era %q is not one the inspector accepts", inspectorProtocolEra)
	}
}

func TestInspectorPinSatisfiesTheLaunchContract(t *testing.T) {
	assertInspectorSpec(t, (inspectorOptions{}).packageSpec())
	if inspectorMinimumMajor < 2 {
		t.Fatalf("inspectorMinimumMajor = %d: an override below major 2 cannot run the generated configuration", inspectorMinimumMajor)
	}
}

func TestInspectorPropagatesRunnerFailure(t *testing.T) {
	boom := errors.New("npx exploded")
	rec := &runRecorder{err: boom}
	cmd, _, _ := newInspector(t, rec)

	err := cmd.Handle(nil, nil)
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the runner failure", err)
	}
	// Even a failed run must not leave the configuration behind.
	if _, statErr := os.Stat(rec.launch.ConfigPath); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("configuration file leaked after a failed run: %v", statErr)
	}
}

func TestInspectorWithoutAServerFails(t *testing.T) {
	rec := &runRecorder{}
	out := &bytes.Buffer{}
	cmd := inspectorCommand{out: out, run: rec.run, binary: func() (string, error) { return "/projects/demo/vel", nil }}

	err := cmd.Handle(nil, nil)
	if err == nil {
		t.Fatal("a command without a server should not run the inspector")
	}
	if !strings.Contains(err.Error(), "inspector command built without a server") {
		t.Fatalf("error = %v, want it to name the missing server", err)
	}
	if rec.calls != 0 {
		t.Fatal("the inspector was launched without a server")
	}
	if out.Len() != 0 {
		t.Fatalf("output = %q, want nothing announced", out.String())
	}
}

func TestInspectorBinaryResolutionFailurePropagates(t *testing.T) {
	failing := func() (string, error) { return "", errors.New("no project binary") }

	t.Run("without a command to fall back on", func(t *testing.T) {
		rec := &runRecorder{}
		cmd, _, _ := newInspector(t, rec)
		cmd.binary = failing

		err := cmd.Handle(nil, nil)
		if err == nil || !strings.Contains(err.Error(), "no project binary") {
			t.Fatalf("error = %v, want the binary resolution failure", err)
		}
		if rec.calls != 0 {
			t.Fatal("the inspector was launched without a command to run")
		}
	})

	// The failure tells the operator to pass --command, so passing it has to be
	// a way out: the binary is never resolved when one was given.
	t.Run("rescued by an explicit command", func(t *testing.T) {
		rec := &runRecorder{}
		cmd, _, _ := newInspector(t, rec)
		cmd.binary = failing

		if err := cmd.Handle(nil, []string{"--command", "/usr/local/bin/demo"}); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if got := rec.config.Servers["demo"].Command; got != "/usr/local/bin/demo" {
			t.Fatalf("command = %q, want the --command override", got)
		}
	})
}

func TestParseInspectorArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    inspectorOptions
		wantErr string
	}{
		{"no flags", nil, inspectorOptions{}, ""},
		{"spaced values", []string{"--url", "http://x/mcp", "--host", "127.0.0.1", "--port", "1"}, inspectorOptions{url: "http://x/mcp", host: "127.0.0.1", port: "1"}, ""},
		{"inline values", []string{"--url=http://x/mcp?a=b", "--port=65535"}, inspectorOptions{url: "http://x/mcp?a=b", port: "65535"}, ""},
		{"last flag wins", []string{"--port", "1", "--port", "2"}, inspectorOptions{port: "2"}, ""},
		{"empty inline value", []string{"--host="}, inspectorOptions{}, ""},
		{"unknown flag", []string{"--force"}, inspectorOptions{}, "unknown flag"},
		{"positional argument", []string{"demo"}, inspectorOptions{}, "unexpected argument"},
		{"missing value", []string{"--url"}, inspectorOptions{}, "--url requires a value"},
		{"port zero", []string{"--port", "0"}, inspectorOptions{}, "--port must be"},
		{"port too large", []string{"--port", "65536"}, inspectorOptions{}, "--port must be"},
		{"port negative", []string{"--port", "-1"}, inspectorOptions{}, "--port must be"},
		{"port not a number", []string{"--port", "6274a"}, inspectorOptions{}, "--port must be"},
		{"host with newline", []string{"--host", "127.0.0.1\nPATH=/evil"}, inspectorOptions{}, "control characters"},
		{"host with nul", []string{"--host", "127.0.0.1\x00"}, inspectorOptions{}, "control characters"},
		{"host with equals", []string{"--host", "a=b"}, inspectorOptions{}, "control characters"},
		{"command with newline", []string{"--command", "vel\nrm -rf /"}, inspectorOptions{}, "control characters"},
		{"ca certificate with nul", []string{"--ca-cert", "ca.pem\x00"}, inspectorOptions{}, "control characters"},
		{"version with shell metacharacters", []string{"--inspector-version", "0.1.0; rm -rf /"}, inspectorOptions{}, "--inspector-version must be"},
		{"version with a slash", []string{"--inspector-version", "../../evil"}, inspectorOptions{}, "--inspector-version must be"},
		{"version too long", []string{"--inspector-version", strings.Repeat("9", 65)}, inspectorOptions{}, "--inspector-version must be"},
		{"version tag accepted", []string{"--inspector-version", "latest"}, inspectorOptions{version: "latest"}, ""},
		// An inspector older than major 2 cannot run the configuration this
		// command writes, so it is refused before anything is launched.
		{"version below the launch contract", []string{"--inspector-version", "0.16.2"}, inspectorOptions{}, "must be 2.0.0 or newer"},
		{"previous major rejected", []string{"--inspector-version", "1.0.2"}, inspectorOptions{}, "must be 2.0.0 or newer"},
		{"first supported major accepted", []string{"--inspector-version", "2.0.0-rc.3"}, inspectorOptions{version: "2.0.0-rc.3"}, ""},
		{"later major accepted", []string{"--inspector-version", "3.1.0"}, inspectorOptions{version: "3.1.0"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseInspectorArgs(tc.args)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error mentioning %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("options = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestInspectorPackageSpecDefaultsToThePinnedVersion(t *testing.T) {
	want := "@modelcontextprotocol/inspector@" + inspectorVersion
	if got := (inspectorOptions{}).packageSpec(); got != want {
		t.Fatalf("package spec = %q, want %q", got, want)
	}
	if got := (inspectorOptions{version: "next"}).packageSpec(); got != "@modelcontextprotocol/inspector@next" {
		t.Fatalf("package spec with an override = %q", got)
	}
}

func TestEnvPairsAreStable(t *testing.T) {
	got := envPairs(map[string]string{"CLIENT_PORT": "1", "HOST": "h", "NODE_EXTRA_CA_CERTS": "/ca.pem"})
	want := []string{"CLIENT_PORT=1", "HOST=h", "NODE_EXTRA_CA_CERTS=/ca.pem"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pairs = %q, want %q", got, want)
	}
	if got := envPairs(nil); len(got) != 0 {
		t.Fatalf("pairs for an empty overlay = %q", got)
	}
}

// Two inspector runs must never share a configuration file, or one would delete
// the other's configuration out from under it.
func TestInspectorConfigFilesAreDistinctPerRun(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := inspectorCommand{
				srv: server.New("demo", "1.0.0"),
				out: &bytes.Buffer{},
				run: func(ctx context.Context, l inspectorLaunch) error {
					mu.Lock()
					defer mu.Unlock()
					if seen[l.ConfigPath] {
						t.Errorf("configuration path %q reused", l.ConfigPath)
					}
					seen[l.ConfigPath] = true
					return nil
				},
				binary: func() (string, error) { return "/projects/demo/vel", nil },
			}
			if err := cmd.Handle(nil, nil); err != nil {
				t.Errorf("Handle: %v", err)
			}
		}()
	}
	wg.Wait()

	if len(seen) != 8 {
		t.Fatalf("got %d distinct configuration files, want 8", len(seen))
	}
}

func TestWriteInspectorConfigRejectsUnencodableConfiguration(t *testing.T) {
	path, err := writeInspectorConfig("demo", map[string]any{"bad": make(chan int)})
	if err == nil {
		_ = os.Remove(path)
		t.Fatal("an unencodable configuration was written, want an error")
	}
	if path != "" {
		t.Fatalf("path = %q, want empty on failure", path)
	}
	if !strings.Contains(err.Error(), "encode inspector configuration") {
		t.Fatalf("error = %v", err)
	}
}

// A guidance write failure aborts the run: the operator would otherwise be left
// with an inspector session and no idea what it is connected to.
func TestInspectorGuidanceWriteFailureAbortsTheRun(t *testing.T) {
	rec := &runRecorder{}
	cmd, _, _ := newInspector(t, rec)
	cmd.out = failingWriter{}

	if err := cmd.Handle(nil, nil); err == nil {
		t.Fatal("a guidance write failure should fail the command")
	}
	if rec.calls != 0 {
		t.Fatal("the inspector was launched after the guidance failed")
	}
}

type failingWriter struct{}

func (failingWriter) Write(p []byte) (int, error) { return 0, errors.New("write failed") }

func TestProjectBinary(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	t.Run("falls back to the running executable", func(t *testing.T) {
		t.Chdir(t.TempDir())
		got, err := projectBinary()
		if err != nil || got != exe {
			t.Fatalf("projectBinary() = (%q,%v), want the running executable %q", got, err, exe)
		}
	})

	t.Run("ignores a non executable file", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)
		if err := os.WriteFile("vel", []byte("#!/bin/sh\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		got, err := projectBinary()
		if err != nil || got != exe {
			t.Fatalf("projectBinary() = (%q,%v), want the running executable", got, err)
		}
	})

	t.Run("ignores a directory", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)
		if err := os.Mkdir("vel", 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		got, err := projectBinary()
		if err != nil || got != exe {
			t.Fatalf("projectBinary() = (%q,%v), want the running executable", got, err)
		}
	})

	t.Run("prefers the project command binary", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)
		if err := os.WriteFile("vel", []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatalf("write: %v", err)
		}
		wd, err := os.Getwd()
		if err != nil {
			t.Fatalf("getwd: %v", err)
		}
		want := filepath.Join(wd, "vel")
		got, err := projectBinary()
		if err != nil || got != want {
			t.Fatalf("projectBinary() = (%q,%v), want %q", got, err, want)
		}
	})
}

// TestInspectorHelperProcess is the child process TestRunInspectorProcess
// starts; it is inert unless that test invokes it.
func TestInspectorHelperProcess(t *testing.T) {
	dump := os.Getenv("MCP_INSPECTOR_TEST_DUMP")
	if dump == "" {
		return
	}

	// The parent takes the dump file as the report that this process is running
	// and cancels the moment it appears, so the interrupt handler is installed
	// before the file is written. Installing it afterwards leaves a window in
	// which the interrupt arrives while the default disposition still holds and
	// terminates this process, losing the report the parent asserts on.
	blocking := os.Getenv("MCP_INSPECTOR_TEST_BLOCK") == "1"
	interrupted := make(chan os.Signal, 1)
	if blocking {
		signal.Notify(interrupted, os.Interrupt)
	}

	body := strings.Join([]string{
		"HOST=" + os.Getenv("HOST"),
		"CLIENT_PORT=" + os.Getenv("CLIENT_PORT"),
		"ARGS=" + strings.Join(os.Args[1:], " "),
	}, "\n")
	if err := os.WriteFile(dump, []byte(body), 0o600); err != nil {
		os.Exit(9)
	}
	if os.Getenv("MCP_INSPECTOR_TEST_FAIL") == "1" {
		os.Exit(3)
	}
	// Stand in for a running inspector: report the interrupt that ends the
	// session, so the parent can tell an interrupt from a kill. The bound keeps
	// a stray child from outliving the test run.
	if blocking {
		select {
		case <-interrupted:
			if err := os.WriteFile(dump+".interrupted", []byte("yes"), 0o600); err != nil {
				os.Exit(9)
			}
		case <-time.After(60 * time.Second):
			os.Exit(9)
		}
	}
	os.Exit(0)
}

// waitForFile blocks until path exists, which the helper process uses to report
// that it is running. It polls rather than sleeping for a fixed duration, and
// fails the test if the child never reports.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the child process never reported itself started (%s)", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRunInspectorProcess(t *testing.T) {
	t.Run("passes the arguments and the environment overlay", func(t *testing.T) {
		dump := filepath.Join(t.TempDir(), "child.txt")
		err := runInspectorProcess(context.Background(), inspectorLaunch{
			Name: os.Args[0],
			Args: []string{"-test.run=TestInspectorHelperProcess", "--", "--config", "/tmp/config.json"},
			Env: map[string]string{
				"HOST":                    "0.0.0.0",
				"CLIENT_PORT":             "6274",
				"MCP_INSPECTOR_TEST_DUMP": dump,
			},
		})
		if err != nil {
			t.Fatalf("runInspectorProcess: %v", err)
		}
		body, readErr := os.ReadFile(dump)
		if readErr != nil {
			t.Fatalf("the child process did not run: %v", readErr)
		}
		for _, want := range []string{"HOST=0.0.0.0", "CLIENT_PORT=6274", "--config /tmp/config.json"} {
			if !strings.Contains(string(body), want) {
				t.Fatalf("child saw %q, want it to contain %q", body, want)
			}
		}
	})

	t.Run("reports a failing inspector", func(t *testing.T) {
		err := runInspectorProcess(context.Background(), inspectorLaunch{
			Name: os.Args[0],
			Args: []string{"-test.run=TestInspectorHelperProcess"},
			Env: map[string]string{
				"MCP_INSPECTOR_TEST_DUMP": filepath.Join(t.TempDir(), "child.txt"),
				"MCP_INSPECTOR_TEST_FAIL": "1",
			},
		})
		if err == nil {
			t.Fatal("a non-zero exit should be reported")
		}
		if !strings.Contains(err.Error(), "the MCP Inspector exited") {
			t.Fatalf("error = %v, want an exit failure, not a start failure", err)
		}
	})

	t.Run("reports an inspector that cannot be started", func(t *testing.T) {
		err := runInspectorProcess(context.Background(), inspectorLaunch{
			Name: filepath.Join(t.TempDir(), "not-installed"),
		})
		if err == nil || !strings.Contains(err.Error(), "start the MCP Inspector") {
			t.Fatalf("error = %v, want a start failure", err)
		}
	})

	// Interrupting the command is how an inspector session ends, so it is not a
	// failure, and the inspector is interrupted rather than killed so it can
	// take its own node children (the UI and proxy) down with it.
	t.Run("a stopped session interrupts the inspector and succeeds", func(t *testing.T) {
		dump := filepath.Join(t.TempDir(), "child.txt")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan error, 1)
		go func() {
			done <- runInspectorProcess(ctx, inspectorLaunch{
				Name: os.Args[0],
				Args: []string{"-test.run=TestInspectorHelperProcess"},
				Env: map[string]string{
					"MCP_INSPECTOR_TEST_DUMP":  dump,
					"MCP_INSPECTOR_TEST_BLOCK": "1",
				},
			})
		}()

		waitForFile(t, dump)
		cancel()

		if err := <-done; err != nil {
			t.Fatalf("a stopped session reported %v, want a clean stop", err)
		}
		if _, err := os.Stat(dump + ".interrupted"); err != nil {
			t.Fatalf("the inspector was not interrupted before it went away: %v", err)
		}
	})

	t.Run("a context cancelled before the start is a clean stop", func(t *testing.T) {
		dump := filepath.Join(t.TempDir(), "child.txt")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := runInspectorProcess(ctx, inspectorLaunch{
			Name: os.Args[0],
			Args: []string{"-test.run=TestInspectorHelperProcess"},
			Env:  map[string]string{"MCP_INSPECTOR_TEST_DUMP": dump},
		})
		if err != nil {
			t.Fatalf("error = %v, want a clean stop", err)
		}
		if _, statErr := os.Stat(dump); !errors.Is(statErr, fs.ErrNotExist) {
			t.Fatal("the inspector ran despite the cancelled context")
		}
	})
}
