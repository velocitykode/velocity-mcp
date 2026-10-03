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
	"slices"
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
		// wantShown is the url the guidance prints: the one the inspector
		// dials, with every query value withheld, since a query value is
		// where an access token travels and the terminal is a log.
		wantShown string
		// wantHint is the certificate guidance the run must print, empty for a
		// url that carries no certificate.
		wantHint string
	}{
		{"http carries no certificate guidance", "http://localhost:8080/mcp", "http://localhost:8080/mcp", "http://localhost:8080/mcp", ""},
		{"https on localhost keeps verification on", "https://localhost:8443/mcp", "https://localhost:8443/mcp", "https://localhost:8443/mcp", localCertHint},
		{"https on a development domain keeps verification on", "https://demo.test/mcp", "https://demo.test/mcp", "https://demo.test/mcp", localCertHint},
		{"https on a remote host keeps verification on", "https://api.example.com/mcp", "https://api.example.com/mcp", "https://api.example.com/mcp", remoteCertHint},
		{"https on an mdns host keeps verification on", "https://mac.local/mcp", "https://mac.local/mcp", "https://mac.local/mcp", remoteCertHint},
		{"query string survives", "http://localhost:8080/mcp?tenant=acme", "http://localhost:8080/mcp?tenant=acme", "http://localhost:8080/mcp?tenant=xxxxx", ""},
		// Several parameters mean an "&" in the url: it must reach the
		// inspector as one ampersand, neither escaped away nor doubled, and
		// the guidance shows the parameters that were passed without their
		// values.
		{"multi parameter query string survives", "http://localhost:8080/mcp?tenant=acme&region=eu", "http://localhost:8080/mcp?tenant=acme&region=eu", "http://localhost:8080/mcp?tenant=xxxxx&region=xxxxx", ""},
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

			wantGuidance := []string{"Transport Type => Streamable HTTP\n", "URL => " + tc.wantShown + "\n"}
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

// The inspector runs under node and is fetched by npm, and both read their
// settings from the environment. Any inherited setting that would change what
// the launch verified and announced is withheld, as a class rather than by
// name: node's own settings (NODE_OPTIONS injects any flag, NODE_PATH and
// NODE_COMPILE_CACHE load code from elsewhere, NODE_EXTRA_CA_CERTS adds a
// trust anchor the guidance never named), npm's (registry, strict-ssl, trust
// store, the config file that could set them), the OpenSSL stack node links,
// and the inspector's own auth and binding switches. Names are compared
// case-folded: Windows resolves variables without regard to case and npm reads
// its settings that way everywhere, so one spelling must not slip through
// where another is blocked.
func TestInspectorEnvironWithholdsInheritedRuntimeSettings(t *testing.T) {
	withheld := []string{
		"NODE_TLS_REJECT_UNAUTHORIZED=0",
		"node_tls_reject_unauthorized=0",
		"Node_TLS_Reject_Unauthorized=0",
		"NODE_OPTIONS=--tls-min-v1.0 --insecure-http-parser --require /tmp/x.js",
		"node_options=--tls-min-v1.0",
		"NODE_EXTRA_CA_CERTS=/tmp/rogue-ca.pem",
		"NODE_PATH=/tmp/modules",
		"NODE_COMPILE_CACHE=/tmp/cache",
		"NODE_USE_ENV_PROXY=1",
		"npm_config_strict_ssl=false",
		"NPM_CONFIG_STRICT_SSL=false",
		"npm_config_strict-ssl=false",
		"Npm_Config_Registry=http://registry.evil.test/",
		"npm_config_registry=http://registry.evil.test/",
		"npm_config_ca=-----BEGIN CERTIFICATE-----",
		"npm_config_cafile=/tmp/rogue-ca.pem",
		"npm_config_userconfig=/tmp/evil.npmrc",
		"npm_config_globalconfig=/tmp/evil.npmrc",
		"OPENSSL_CONF=/tmp/openssl.cnf",
		"openssl_conf=/tmp/openssl.cnf",
		"SSL_CERT_FILE=/tmp/rogue-ca.pem",
		"SSL_CERT_DIR=/tmp/rogue-certs",
		"DANGEROUSLY_OMIT_AUTH=true",
		"dangerously_omit_auth=true",
		"MCP_PROXY_AUTH_TOKEN=known-token",
		"ALLOWED_ORIGINS=*",
		"HOST=0.0.0.0",
		"Host=0.0.0.0",
		"MCP_PROXY_FULL_ADDRESS=http://attacker.test:6277",
	}
	inherited := []string{
		"PATH=/usr/bin",
		"HOME=/home/dev",
		"MCP_INSPECTOR_TEST_MARKER=inherited",
		"CLIENT_PORT=6000",
		"SERVER_PORT=6277",
		"MCP_SERVER_REQUEST_TIMEOUT=10000",
		"MCP_AUTO_OPEN_ENABLED=false",
		"HTTPS_PROXY=http://proxy.corp.test:3128",
		"NPM_TOKEN=secret",
		"npm_execpath=/usr/lib/node_modules/npm/bin/npm-cli.js",
		"NODEJS_HOME=/opt/node",
		"MY_NODE_OPTIONS=x",
		"HOSTNAME=dev-box",
	}
	parent := append(append([]string{}, withheld...), inherited...)

	got := inspectorEnviron(parent, nil)
	for _, entry := range withheld {
		if slices.Contains(got, entry) {
			t.Errorf("inherited %q reached the inspector; it must be withheld", entry)
		}
	}
	for _, entry := range inherited {
		if !slices.Contains(got, entry) {
			t.Errorf("inherited %q was dropped; only runtime, npm, TLS stack and inspector security settings are withheld", entry)
		}
	}
	if len(got) != len(inherited) {
		t.Errorf("child environment = %q, want exactly the inherited entries", got)
	}
}

// A variable the launch sets itself replaces every inherited spelling of it,
// so the child reads one value and it is the one the command decided on.
func TestInspectorEnvironOverlayReplacesInheritedSpellings(t *testing.T) {
	parent := []string{"client_port=1", "Client_Port=2", "CLIENT_PORT=3", "PATH=/usr/bin", "host=inherited"}
	overlay := map[string]string{"CLIENT_PORT": "6274", "HOST": "127.0.0.1", "NODE_EXTRA_CA_CERTS": "/ca.pem"}

	got := inspectorEnviron(parent, overlay)
	want := []string{"PATH=/usr/bin", "CLIENT_PORT=6274", "HOST=127.0.0.1", "NODE_EXTRA_CA_CERTS=/ca.pem"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("child environment = %q, want %q", got, want)
	}
}

// A --url may carry an access token: as the password, as the user name (the
// usual form for a bearer token in a url), in a query parameter or in the
// fragment. The inspector needs the url whole, so the configuration file
// carries it as written; the guidance printed to the terminal, which ends up
// in scrollback and CI logs, must not.
func TestInspectorGuidanceWithholdsURLSecrets(t *testing.T) {
	const full = "https://admin:hunter2@mcp.example.com:8443/mcp/v1?api_key=sk-live-123&tenant=acme&bare-token#frag-token"
	rec := &runRecorder{}
	cmd, out, _ := newInspector(t, rec)

	if err := cmd.Handle(nil, []string{"--url", full}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := rec.config.Servers["demo"].URL; got != full {
		t.Fatalf("configured url = %q, want the url as written", got)
	}

	printed := out.String()
	for _, secret := range []string{"admin", "hunter2", "sk-live-123", "acme", "bare-token", "frag-token"} {
		if strings.Contains(printed, secret) {
			t.Errorf("the guidance printed %q:\n%s", secret, printed)
		}
	}
	want := "URL => https://xxxxx@mcp.example.com:8443/mcp/v1?api_key=xxxxx&tenant=xxxxx&xxxxx#xxxxx\n"
	if !strings.Contains(printed, want) {
		t.Errorf("guidance = %q, want it to carry %q", printed, want)
	}
	// A url without secrets is printed as it is, so the operator can check it.
	rec, out = &runRecorder{}, nil
	cmd, out, _ = newInspector(t, rec)
	if err := cmd.Handle(nil, []string{"--url", "https://mcp.example.com/mcp"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.String(), "URL => https://mcp.example.com/mcp\n") {
		t.Errorf("guidance = %q, want the plain url", out.String())
	}
}

// No error, guidance line or question of the command may show an argument
// value that can carry a url credential. The operator can put a url after any
// flag, or after none, so the rule is exercised as a class: every value below
// is passed in every position the command line has, and whatever the command
// then says, in its error, on its output or in a question it asks, must hold
// none of the value's secrets.
func TestInspectorNeverShowsAnArgumentThatMayCarryCredentials(t *testing.T) {
	values := []struct {
		name  string
		value string
		// secrets are the parts of value that must never be shown.
		secrets []string
		// program reports a value that is not a url by its text and could
		// name a program; --command prints the program it is given, which is
		// what that flag is for, so such a value is not passed to it.
		program bool
	}{
		{name: "a url with credentials everywhere", value: "https://admin:hunter2@mcp.example.com:8443/mcp/v1?api_key=sk-live-123&tenant=acme&bare-token#frag-token",
			secrets: []string{"admin", "hunter2", "sk-live-123", "acme", "bare-token", "frag-token"}},
		{name: "a wrong scheme", value: "ftp://admin:hunter2@files.example.com/x?token=sk-live-123#frag-token", secrets: []string{"admin", "hunter2", "sk-live-123", "frag-token"}},
		{name: "no host", value: "http://admin:hunter2@/mcp?token=sk-live-123", secrets: []string{"admin", "hunter2", "sk-live-123"}},
		// A url without "//" has no host; everything after the scheme is one
		// opaque part, and a secret written there is as secret as anywhere.
		{name: "an opaque url", value: "https:admin:hunter2@mcp.example.com/mcp?token=sk-live-123#frag-token", secrets: []string{"admin", "hunter2", "sk-live-123", "frag-token"}},
		{name: "no scheme, with a password", value: "admin:hunter2@mcp.example.com/mcp?token=sk-live-123", secrets: []string{"admin", "hunter2", "sk-live-123"}, program: true},
		{name: "no scheme, with a token as the user", value: "sk-live-123@mcp.example.com/mcp", secrets: []string{"sk-live-123"}, program: true},
		{name: "a network path", value: "//admin:hunter2@mcp.example.com/mcp", secrets: []string{"admin", "hunter2"}, program: true},
		{name: "a secret where the port goes", value: "https://admin@mcp.example.com:hunter2/mcp", secrets: []string{"admin", "hunter2"}},
		{name: "a broken escape in the password", value: "https://admin:hunter2%zz@mcp.example.com/mcp?token=sk-live-123", secrets: []string{"admin", "hunter2", "%zz", "sk-live-123"}},
		{name: "a broken escape in the path", value: "https://admin:hunter2@mcp.example.com/%zz?token=sk-live-123", secrets: []string{"admin", "hunter2", "sk-live-123"}},
		{name: "an unterminated route parameter", value: "https://admin:hunter2@mcp.example.com/mcp/{org?token=sk-live-123", secrets: []string{"admin", "hunter2", "sk-live-123"}},
		{name: "an unnamed route parameter", value: "https://admin:hunter2@mcp.example.com/{}?token=sk-live-123", secrets: []string{"admin", "hunter2", "sk-live-123"}},
		{name: "braces in the password", value: "https://admin:hu{nter2}x@mcp.example.com/mcp", secrets: []string{"admin", "nter2"}},
		{name: "a control character", value: "https://admin:hunter2@mcp.example.com/\x7fmcp?token=sk-live-123", secrets: []string{"admin", "hunter2", "sk-live-123"}},
		{name: "a query behind an escaped question mark", value: "https://mcp.example.com/mcp%3Ftoken=sk-live-123", secrets: []string{"sk-live-123"}},
		{name: "path parameters", value: "https://mcp.example.com/mcp;token=sk-live-123/v1;session=hunter2", secrets: []string{"sk-live-123", "hunter2"}},
		{name: "an escaped separator in a query key", value: "https://mcp.example.com/mcp?a%3Dsk-live-123=1&b%26token%3Dhunter2=2", secrets: []string{"sk-live-123", "hunter2"}},
		{name: "a query split at semicolons", value: "https://mcp.example.com/mcp?sk-live-123;x=hunter2&api_key=frag-token;bare-token", secrets: []string{"sk-live-123", "hunter2", "frag-token", "bare-token"}},
	}
	placements := []struct {
		name string
		args func(value string) []string
		// command reports the placement that hands the value to --command.
		command bool
	}{
		{name: "after --url", args: func(v string) []string { return []string{"--url", v} }},
		{name: "inline with --url", args: func(v string) []string { return []string{"--url=" + v} }},
		{name: "after a flag in the wrong case", args: func(v string) []string { return []string{"--Url", v} }},
		{name: "inline with a flag in the wrong case", args: func(v string) []string { return []string{"--Url=" + v} }},
		{name: "as a flag", args: func(v string) []string { return []string{"--" + v} }},
		{name: "as a stray argument", args: func(v string) []string { return []string{v} }},
		{name: "as a stray argument after a flag", args: func(v string) []string { return []string{"--port", "6274", v} }},
		{name: "after --port", args: func(v string) []string { return []string{"--port", v} }},
		{name: "inline with --port", args: func(v string) []string { return []string{"--port=" + v} }},
		{name: "after --inspector-version", args: func(v string) []string { return []string{"--inspector-version", v} }},
		{name: "after --ca-cert", args: func(v string) []string { return []string{"--ca-cert", v} }},
		{name: "after --host", args: func(v string) []string { return []string{"--host", v} }},
		{name: "after --command", args: func(v string) []string { return []string{"--command", v} }, command: true},
	}
	for _, value := range values {
		for _, placement := range placements {
			if value.program && placement.command {
				continue
			}
			t.Run(value.name+"/"+placement.name, func(t *testing.T) {
				rec := &runRecorder{}
				cmd, out, asked := newInspector(t, rec, "value")
				err := cmd.Handle(nil, placement.args(value.value))

				shown := map[string]string{"output": out.String(), "questions": strings.Join(*asked, "\n")}
				if err != nil {
					shown["error"] = err.Error()
				}
				for where, text := range shown {
					for _, secret := range value.secrets {
						if strings.Contains(text, secret) {
							t.Errorf("the %s shows %q: %s", where, secret, text)
						}
					}
				}
			})
		}
	}
}

// The refusals still say what is wrong and where, without the value.
func TestInspectorArgumentErrorsNameTheProblemWithoutTheValue(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"a flag in the wrong case", []string{"--Url=https://admin:hunter2@h/mcp?api_key=sk-live-123"}, `unknown flag "--Url" (the value is not shown`},
		{"a url as a flag", []string{"--port", "6274", "--https://admin:hunter2@h/mcp"}, "unknown flag at position 3 (the value is not shown"},
		{"a plain unknown flag", []string{"--force"}, `unknown flag "--force" (the value is not shown`},
		{"a stray argument", []string{"--port", "6274", "https://admin:hunter2@h/mcp"}, "unexpected argument at position 3 (the value is not shown"},
		{"a port", []string{"--port", "https://admin:hunter2@h/mcp"}, "--port must be a number between 1 and 65535 (the value is not shown"},
		{"a version", []string{"--inspector-version", "https://admin:hunter2@h/mcp"}, "--inspector-version must be an npm version or tag (the value is not shown"},
		{"a version that is too old", []string{"--inspector-version", "1.9.0"}, "--inspector-version must be 2.0.0 or newer, got major version 1:"},
		{"a certificate that is not there", []string{"--ca-cert", "https://admin:hunter2@h/mcp"}, "--ca-cert must name a readable certificate file (the value is not shown, since an argument may carry a url with credentials): no such file or directory"},
		{"a certificate that is a directory", []string{"--ca-cert", dir}, "--ca-cert must name a regular file (the value is not shown"},
		{"a url as the command", []string{"--command", "https://admin:hunter2@h/mcp"}, "--command must name a program, not a url; pass a url with --url (the value is not shown"},
		{"an opaque url as the command", []string{"--command", "HTTPS:admin:hunter2@h/mcp"}, "--command must name a program, not a url; pass a url with --url (the value is not shown"},
		{"a wrong scheme", []string{"--url", "ftp://admin:hunter2@h/x"}, "mcp: --url must be an absolute http or https url (the value is not shown"},
		{"no host", []string{"--url", "http://admin:hunter2@/mcp"}, "mcp: --url is missing a host (the value is not shown"},
		{"an invalid url", []string{"--url", "https://admin@h:hunter2/mcp"}, "mcp: --url is not a valid url (the value is not shown"},
		{"an unterminated route parameter", []string{"--url", "https://h/mcp/{org"}, "mcp: unterminated route parameter in --url (the value is not shown"},
		{"an unnamed route parameter", []string{"--url", "https://h/mcp/{}"}, "mcp: unnamed route parameter in --url (the value is not shown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &runRecorder{}
			cmd, _, _ := newInspector(t, rec, "value")
			err := cmd.Handle(nil, tc.args)
			if err == nil {
				t.Fatalf("arguments %q accepted, want an error", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to say %q", err, tc.want)
			}
			if rec.calls != 0 {
				t.Fatal("the inspector was launched with refused arguments")
			}
		})
	}
}

// The url shown in the guidance keeps the query keys and the separators as
// written and replaces every value, whichever separator the parameters are
// told apart by.
func TestRedactedQuery(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  string
	}{
		{"one parameter", "api_key=sk-live-123", "api_key=xxxxx"},
		{"two parameters", "a=1&b=2", "a=xxxxx&b=xxxxx"},
		{"a bare parameter", "sk-live-123", "xxxxx"},
		{"semicolons", "a=1;b=2", "a=xxxxx;b=xxxxx"},
		{"a bare parameter before a semicolon", "sk-live-123;x=hunter2&api_key=frag", "xxxxx;x=xxxxx&api_key=xxxxx"},
		{"a bare parameter after a semicolon", "x=1;sk-live-123", "x=xxxxx;xxxxx"},
		{"an empty value", "a=&b=2", "a=xxxxx&b=xxxxx"},
		{"empty parameters", "a=1&&b=2;", "a=xxxxx&&b=xxxxx;"},
		{"a value holding an equals sign", "a=b=c", "a=xxxxx"},
		{"only separators", "&;", "&;"},
		{"a key with an escape", "a%3Dsk-live-123=1", "xxxxx=xxxxx"},
		{"a key with brackets", "filter[name]=x&plain.key_1~-=y", "xxxxx=xxxxx&plain.key_1~-=xxxxx"},
		{"an empty key", "=x", "=xxxxx"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactedQuery(tc.query); got != tc.want {
				t.Fatalf("redactedQuery(%q) = %q, want %q", tc.query, got, tc.want)
			}
		})
	}
}

// Route parameters are looked for after the user information only, wherever
// the url puts it.
func TestUserInfoEnd(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		// want is the text before the index reported.
		want string
	}{
		{"no user information", "https://mcp.example.com/mcp/{org}", ""},
		{"a user and a password", "https://admin:p{ss}w@mcp.example.com/{org}", "https://admin:p{ss}w@"},
		{"an at sign in the password", "https://admin:p@ss@mcp.example.com/mcp", "https://admin:p@ss@"},
		{"an at sign in the path only", "https://mcp.example.com/mcp/@{org}", ""},
		{"an at sign in the query only", "https://mcp.example.com/mcp?to=a@b", ""},
		{"no scheme", "admin:p{ss}w@mcp.example.com/mcp", "admin:p{ss}w@"},
		{"a network path", "//admin:p{ss}w@mcp.example.com/mcp", "//admin:p{ss}w@"},
		{"an opaque url", "https:admin:p{ss}w@mcp.example.com/mcp", "https:admin:p{ss}w@"},
		{"two slashes later in the path", "/mcp//admin@x/{org}", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.raw[:userInfoEnd(tc.raw)]; got != tc.want {
				t.Fatalf("userInfoEnd(%q) ends after %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// The route is shown in the guidance so the operator can check it, but only
// the segments that are plain text: a segment holding an escape or a
// delimiter may carry a parameter for a parser that splits the url
// differently, and is withheld like a value.
func TestInspectorGuidanceShowsOnlyPlainPathSegments(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"a plain route", "https://mcp.example.com/mcp/v1.2/tenant_a~b-c", "https://mcp.example.com/mcp/v1.2/tenant_a~b-c"},
		{"no path", "https://mcp.example.com", "https://mcp.example.com"},
		{"a trailing slash", "https://mcp.example.com/mcp/", "https://mcp.example.com/mcp/"},
		{"an escaped question mark", "https://mcp.example.com/mcp%3Ftoken=sk-live-123/v1", "https://mcp.example.com/xxxxx/v1"},
		{"path parameters", "https://mcp.example.com/mcp;token=sk-live-123/v1", "https://mcp.example.com/xxxxx/v1"},
		{"an escaped slash", "https://mcp.example.com/mcp/acme%2Feu", "https://mcp.example.com/mcp/xxxxx"},
		{"an at sign", "https://mcp.example.com/mcp/admin:hunter2@x", "https://mcp.example.com/mcp/xxxxx"},
		{"an equals sign", "https://mcp.example.com/mcp/token=sk-live-123", "https://mcp.example.com/mcp/xxxxx"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &runRecorder{}
			cmd, out, _ := newInspector(t, rec)
			if err := cmd.Handle(nil, []string{"--url", tc.url}); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if got := rec.config.Servers["demo"].URL; got != tc.url {
				t.Fatalf("configured url = %q, want the url as written", got)
			}
			if want := "URL => " + tc.want + "\n"; !strings.Contains(out.String(), want) {
				t.Fatalf("guidance = %q, want it to carry %q", out.String(), want)
			}
		})
	}
}
