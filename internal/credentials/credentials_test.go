package credentials

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunAskpass(t *testing.T) {
	env := map[string]string{EnvAskpassUser: "x-access-token", EnvAskpassToken: "dummy-token-123"}
	get := func(k string) string { return env[k] }

	var b bytes.Buffer
	if err := RunAskpass([]string{"Username for 'https://github.com': "}, get, &b); err != nil {
		t.Fatal(err)
	}
	if b.String() != "x-access-token\n" {
		t.Fatalf("username answer = %q", b.String())
	}
	b.Reset()
	if err := RunAskpass([]string{"Password for 'https://x-access-token@github.com': "}, get, &b); err != nil {
		t.Fatal(err)
	}
	if b.String() != "dummy-token-123\n" {
		t.Fatalf("password answer = %q", b.String())
	}
	if err := RunAskpass([]string{"Passphrase for key"}, get, &b); err == nil {
		t.Fatal("expected unsupported prompt error")
	}
	if err := RunAskpass(nil, get, &b); err == nil {
		t.Fatal("expected missing prompt error")
	}
}

func TestEnvNilAndSet(t *testing.T) {
	var a *Auth
	if a.Env("/bin/x") != nil {
		t.Fatal("nil auth must yield no env")
	}
	e := (&Auth{Token: "tok-abcdef"}).Env("/bin/helper")
	joined := strings.Join(e, "\n")
	if !strings.Contains(joined, "GIT_ASKPASS=/bin/helper") || !strings.Contains(joined, EnvAskpassToken+"=tok-abcdef") {
		t.Fatalf("unexpected env: %v", e)
	}
}

func TestRedact(t *testing.T) {
	in := "fatal: unable to access 'https://x-access-token:ghp_SECRET1234@github.com/o/r.git/': 403 token ghp_SECRET1234"
	out := Redact(in, "ghp_SECRET1234")
	if strings.Contains(out, "SECRET1234") || strings.Contains(out, "x-access-token") {
		t.Fatalf("secret leaked: %s", out)
	}
	if !strings.Contains(Redact("https://user@github.com/x"), "https://***@github.com/x") {
		t.Fatal("userinfo not redacted")
	}
}
