package setup

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Asker asks one question interactively. Implementations must not echo a
// secret answer.
type Asker interface {
	Ask(q Question, def string) (string, error)
}

// Resolve runs the engine: for each applicable question, an existing env value
// wins (R1), then a supplied answer, then the asker, then the default; Mint
// questions left blank are minted. It returns the full env (existing keys
// preserved). stdin backs the "-" indirection.
func Resolve(existing, answers map[string]string, ask Asker, stdin io.Reader) (map[string]string, error) {
	env := map[string]string{}
	for k, v := range existing {
		env[k] = v
	}
	a := map[string]string{}
	for _, q := range Questions() {
		if q.When != nil && !q.When(a) {
			continue
		}
		if v, ok := existing[q.Keys[0]]; ok {
			a[q.ID] = v
			continue
		}
		v, given, err := supplied(q, answers, stdin)
		if err != nil {
			return nil, err
		}
		if !given && ask != nil {
			def := ""
			if q.Default != nil {
				def = q.Default(a)
			}
			if v, err = ask.Ask(q, def); err != nil {
				return nil, err
			}
			if v == "" {
				v = def
			}
			given = true
		}
		if !given && q.Default != nil {
			v = q.Default(a)
		}
		if v == "" && q.Mint {
			if v, err = mint(); err != nil {
				return nil, err
			}
		}
		if err := q.Validate(v); err != nil {
			return nil, err
		}
		a[q.ID] = v
		if q.Write != nil {
			for k, x := range q.Write(v, a) {
				env[k] = x
			}
		} else {
			env[q.Keys[0]] = v
		}
	}
	return env, nil
}

// supplied resolves an answer from the answers map or VIZRA_SETUP_<ID>, with
// "@path" (file) and "-" (stdin) indirections.
func supplied(q Question, answers map[string]string, stdin io.Reader) (string, bool, error) {
	v, ok := answers[q.ID]
	if !ok {
		v, ok = os.LookupEnv("VIZRA_SETUP_" + strings.ToUpper(q.ID))
	}
	if !ok {
		return "", false, nil
	}
	switch {
	case v == "-":
		if stdin == nil {
			return "", false, fmt.Errorf("%s: '-' given but no stdin", q.ID)
		}
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", false, fmt.Errorf("%s: read stdin: %w", q.ID, err)
		}
		v = strings.TrimRight(line, "\r\n")
	case strings.HasPrefix(v, "@"):
		b, err := os.ReadFile(v[1:])
		if err != nil {
			return "", false, fmt.Errorf("%s: read %s: %w", q.ID, v[1:], err)
		}
		v = strings.TrimRight(string(b), "\r\n")
	}
	return v, true, nil
}

// Check validates env against every applicable question and returns one
// message per bad or missing key.
func Check(env map[string]string) []string {
	var errs []string
	a := map[string]string{}
	for _, q := range Questions() {
		if q.When != nil && !q.When(a) {
			continue
		}
		v, ok := env[q.Keys[0]]
		if !ok {
			errs = append(errs, q.Keys[0]+": missing")
			continue
		}
		if err := q.Validate(v); err != nil {
			errs = append(errs, q.Keys[0]+": "+strings.TrimPrefix(err.Error(), q.ID+": "))
		}
		a[q.ID] = v
	}
	return errs
}

// rotateRefused lists minted secrets whose rotation cannot be made safe by
// editing the env file alone. They are refused in every mode, --yes-i-know
// included.
var rotateRefused = map[string]string{
	"mfa_kek": "existing TOTP enrolments are wrapped with the current KEK and re-wrapping is not implemented; " +
		"rotating it would make every enrolment unreadable. Use `vizra mfa rewrap-kek` once it exists (VZ-INSTALL-002 follow-up)",
	"postgres_password": "the bundled PostgreSQL keeps its own password in its data volume; changing the env value " +
		"does not change it and locks api/worker/migrate out. Change it in the database first (ALTER ROLE), then edit the env file",
	"clickhouse_password": "the bundled ClickHouse keeps its own password; changing the env value alone breaks analytics writes. " +
		"Change it in ClickHouse first, then edit the env file",
}

// Rotate replaces one minted secret. Requires yes. Secrets in rotateRefused
// are always refused (yesIKnow does not override a data-loss refusal).
// session_secret: logs every user out (documented, allowed). search_hmac: the
// meta compose interpolates the single SEARCH_HMAC_KEY into both api and
// search, so one env file feeds both; restart them together.
func Rotate(env map[string]string, id string, yes, yesIKnow bool) (map[string]string, error) {
	_ = yesIKnow
	q := Lookup(id)
	if !q.Mint {
		return nil, fmt.Errorf("--rotate %s: only generated secrets can be rotated", id)
	}
	if why, ok := rotateRefused[id]; ok {
		return nil, fmt.Errorf("--rotate %s: refused: %s", id, why)
	}
	if !yes {
		return nil, fmt.Errorf("--rotate %s: refusing without --yes", id)
	}
	v, err := mint()
	if err != nil {
		return nil, err
	}
	n := map[string]string{}
	for k, x := range env {
		n[k] = x
	}
	n[q.Keys[0]] = v
	return n, nil
}

// Options drives Run.
type Options struct {
	Path    string
	Answers map[string]string
	Ask     Asker
	Stdin   io.Reader
	Out     io.Writer
	Yes     bool
}

// Run loads Path (if present), resolves, and writes atomically with mode 0600.
// An existing file with permissions broader than 0600 is refused. An existing
// file is rewritten only with Yes; an unchanged result is a no-op.
func Run(o Options) error {
	if o.Out == nil {
		o.Out = io.Discard
	}
	existing, err := ReadEnvFile(o.Path)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	env, err := Resolve(existing, o.Answers, o.Ask, o.Stdin)
	if err != nil {
		return err
	}
	return write(o, existing, env, exists)
}

func write(o Options, existing, env map[string]string, exists bool) error {
	out := Render(env)
	if exists {
		if Render(existing) == out {
			if old, _ := os.ReadFile(o.Path); string(old) == out {
				fmt.Fprintf(o.Out, "%s: unchanged\n", o.Path)
				return nil
			}
		}
		if !o.Yes {
			return fmt.Errorf("%s exists and would change; re-run with --yes to rewrite it", o.Path)
		}
	}
	if err := os.MkdirAll(filepath.Dir(o.Path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(o.Path), ".production.env.*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(out); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), o.Path); err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "%s: wrote %d keys (secrets not shown)\n", o.Path, len(env))
	return nil
}

// WriteEnv writes env to o.Path under the same rules as Run.
func WriteEnv(o Options, env map[string]string) error {
	existing, err := ReadEnvFile(o.Path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	return write(o, existing, env, err == nil)
}

// ReadEnvFile parses KEY=VALUE lines. A file with permissions broader than
// 0600 is refused: it may already have leaked its secrets.
func ReadEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s has mode %v; refusing to use a secrets file readable by others (chmod 600 it)", path, st.Mode().Perm())
	}
	env := map[string]string{}
	sc := bufio.NewScanner(io.LimitReader(f, 1<<20))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s: malformed line (no '=')", path)
		}
		env[strings.TrimSpace(k)] = v
	}
	return env, sc.Err()
}

// Render serialises env deterministically: question keys in interview order,
// then every other key sorted.
func Render(env map[string]string) string {
	var b strings.Builder
	b.WriteString("# Written by `vizra setup`. Re-run it to change answers; existing values are kept.\n")
	done := map[string]bool{}
	for _, q := range Questions() {
		for _, k := range q.Keys {
			if v, ok := env[k]; ok && !done[k] {
				fmt.Fprintf(&b, "%s=%s\n", k, v)
				done[k] = true
			}
		}
	}
	var rest []string
	for k := range env {
		if !done[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		fmt.Fprintf(&b, "%s=%s\n", k, env[k])
	}
	return b.String()
}

// LineAsker is the terminal interview. ReadSecret must read without echo.
type LineAsker struct {
	In         *bufio.Reader
	Out        io.Writer
	ReadSecret func() (string, error)
}

// Ask prompts for q. Secret answers go through ReadSecret, never echoed.
func (l *LineAsker) Ask(q Question, def string) (string, error) {
	for {
		shown := def
		if q.Secret && def != "" {
			shown = "(keep)"
		}
		if shown != "" {
			fmt.Fprintf(l.Out, "%s [%s]: ", q.Prompt, shown)
		} else {
			fmt.Fprintf(l.Out, "%s: ", q.Prompt)
		}
		var v string
		var err error
		eof := false
		if q.Secret {
			v, err = l.ReadSecret()
			fmt.Fprintln(l.Out)
		} else {
			v, err = l.In.ReadString('\n')
			eof = errors.Is(err, io.EOF)
			if eof {
				err = nil
			}
		}
		if err != nil {
			return "", err
		}
		v = strings.TrimSpace(v)
		probe := v
		if probe == "" {
			probe = def
		}
		if probe == "" && q.Mint {
			return "", nil
		}
		if err := q.Validate(probe); err != nil {
			if eof {
				return "", fmt.Errorf("%w (input closed)", err)
			}
			fmt.Fprintln(l.Out, err.Error())
			continue
		}
		return v, nil
	}
}
