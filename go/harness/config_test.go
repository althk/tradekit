package harness

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// parityConfig is the shape the config parity block describes. The Python
// suite declares the same one as a dataclass.
type parityConfig struct {
	Name   string `toml:"name" env:"TK_NAME" validate:"required"`
	Broker struct {
		APIKey    Secret `toml:"api_key" env:"TK_BROKER_API_KEY" validate:"required"`
		APISecret Secret `toml:"api_secret" env:"TK_BROKER_API_SECRET"`
	} `toml:"broker"`
	Risk struct {
		MaxPositions int     `toml:"max_positions" env:"TK_RISK_MAX_POSITIONS"`
		RiskFraction float64 `toml:"risk_fraction"`
	} `toml:"risk"`
}

func (c parityConfig) flat() map[string]any {
	return map[string]any{
		"name":               c.Name,
		"broker.api_key":     c.Broker.APIKey.Reveal(),
		"broker.api_secret":  c.Broker.APISecret.Reveal(),
		"risk.max_positions": float64(c.Risk.MaxPositions), // JSON numbers decode as float64
		"risk.risk_fraction": c.Risk.RiskFraction,
	}
}

type configFixture struct {
	Config struct {
		EnvNames map[string]string `json:"env_names"`
		Required []string          `json:"required"`
		Cases    []struct {
			Name        string            `json:"name"`
			TOML        string            `json:"toml"`
			Env         map[string]string `json:"env"`
			Want        map[string]any    `json:"want"`
			WantMissing []string          `json:"want_missing"`
		} `json:"cases"`
	} `json:"config"`
}

func writeTOML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParityConfig(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "contracts", "testdata", "parity.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f configFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Config.Cases) == 0 {
		t.Fatal("the fixture's config block is empty")
	}

	for _, c := range f.Config.Cases {
		t.Run(c.Name, func(t *testing.T) {
			// Every variable the block names is cleared first, so a value
			// left in the developer's shell cannot decide the outcome.
			for _, name := range f.Config.EnvNames {
				t.Setenv(name, "")
			}
			for k, v := range c.Env {
				t.Setenv(k, v)
			}
			var cfg parityConfig
			err := Load(writeTOML(t, c.TOML), &cfg)

			if len(c.WantMissing) > 0 {
				if err == nil {
					t.Fatalf("expected an error naming %v", c.WantMissing)
				}
				for _, name := range c.WantMissing {
					if !strings.Contains(err.Error(), name) {
						t.Errorf("the error must name every missing field, %q is absent from %q", name, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			got := cfg.flat()
			for k, want := range c.Want {
				if got[k] != want {
					t.Errorf("%s = %v, want %v; Go and Python must resolve the same file and environment identically", k, got[k], want)
				}
			}
		})
	}
}

type sample struct {
	Name    string        `toml:"name" env:"SAMPLE_NAME" validate:"required"`
	Token   Secret        `toml:"token" env:"SAMPLE_TOKEN" validate:"required"`
	Retries int           `toml:"retries" env:"SAMPLE_RETRIES" validate:"required"`
	Timeout time.Duration `toml:"timeout" env:"SAMPLE_TIMEOUT"`
	Debug   bool          `toml:"debug" env:"SAMPLE_DEBUG"`
	Nested  struct {
		Ratio float64 `toml:"ratio" env:"SAMPLE_RATIO"`
	} `toml:"nested"`
}

func TestEnvOverrideWinsAndAnUnsetVariableDoesNotBlankTheField(t *testing.T) {
	t.Setenv("SAMPLE_NAME", "")
	t.Setenv("SAMPLE_TOKEN", "from-env")
	t.Setenv("SAMPLE_RETRIES", "9")
	t.Setenv("SAMPLE_TIMEOUT", "1m30s")
	t.Setenv("SAMPLE_DEBUG", "true")
	t.Setenv("SAMPLE_RATIO", "0.25")
	var cfg sample
	err := Load(writeTOML(t, "name = \"from-file\"\ntoken = \"file-token\"\nretries = 2\n"), &cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Name != "from-file" {
		t.Errorf("an unset variable must leave the file's value alone, got %q", cfg.Name)
	}
	if cfg.Token.Reveal() != "from-env" || cfg.Retries != 9 || cfg.Timeout != 90*time.Second || !cfg.Debug || cfg.Nested.Ratio != 0.25 {
		t.Errorf("env overrides must win for every supported kind, got %s", Redacted(cfg))
	}
}

func TestAnUnparseableOverrideNamesTheVariable(t *testing.T) {
	t.Setenv("SAMPLE_RETRIES", "many")
	var cfg sample
	err := Load(writeTOML(t, "name = \"x\"\ntoken = \"y\"\nretries = 1\n"), &cfg)
	if err == nil || !strings.Contains(err.Error(), "SAMPLE_RETRIES") {
		t.Errorf("a bad value must fail at load and say which variable, got %v", err)
	}
}

func TestMissingRequiredFieldsAreAllNamedInOneError(t *testing.T) {
	for _, name := range []string{"SAMPLE_NAME", "SAMPLE_TOKEN", "SAMPLE_RETRIES"} {
		t.Setenv(name, "")
	}
	var cfg sample
	err := Load(writeTOML(t, "debug = true\n"), &cfg)
	if err == nil {
		t.Fatal("a missing required field must fail at load, not at first use")
	}
	for _, want := range []string{"name", "token", "retries"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must list every missing field so one restart fixes them all; %q is missing from %q", want, err)
		}
	}
}

func TestLoadRejectsANonStruct(t *testing.T) {
	var s string
	if err := Load("nowhere.toml", &s); err == nil {
		t.Error("Load must refuse anything but a pointer to a struct")
	}
	if err := Load("nowhere.toml", sample{}); err == nil {
		t.Error("Load must refuse a struct passed by value; it could not fill it")
	}
}

type creds struct {
	User string
	Pass Secret
}

func TestSecretDoesNotLeakThroughAnyFormattingPath(t *testing.T) {
	c := creds{User: "ops", Pass: Secret("hunter2")}
	for name, rendered := range map[string]string{
		"%v":       fmt.Sprintf("%v", c),
		"%+v":      fmt.Sprintf("%+v", c),
		"%s":       fmt.Sprintf("%s", c.Pass),
		"%#v":      fmt.Sprintf("%#v", c),
		"Sprint":   fmt.Sprint(c.Pass),
		"Redacted": Redacted(c),
	} {
		if strings.Contains(rendered, "hunter2") {
			t.Errorf("%s leaked the secret: %s", name, rendered)
		}
		if !strings.Contains(rendered, Redaction) {
			t.Errorf("%s must show the redaction marker, got %s", name, rendered)
		}
	}

	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "hunter2") || !strings.Contains(string(raw), "redacted") {
		t.Errorf("json.Marshal on a containing struct leaked the secret: %s", raw)
	}

	var buf strings.Builder
	slog.New(slog.NewTextHandler(&buf, nil)).Info("start", "pass", c.Pass)
	if strings.Contains(buf.String(), "hunter2") {
		t.Errorf("slog leaked the secret: %s", buf.String())
	}

	if c.Pass.Reveal() != "hunter2" {
		t.Error("Reveal must return the real value; it is the one supported way to read one")
	}
}

func TestSecretRoundTripsThroughJSONInput(t *testing.T) {
	var c creds
	if err := json.Unmarshal([]byte(`{"User":"ops","Pass":"hunter2"}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.Pass.Reveal() != "hunter2" {
		t.Errorf("a secret must be readable from JSON input, got %q", c.Pass.Reveal())
	}
}

func TestRedactedMasksUnexportedSecretsToo(t *testing.T) {
	type hidden struct {
		Public string
		secret Secret
		Inner  *creds
	}
	h := hidden{Public: "p", secret: Secret("hunter2"), Inner: &creds{User: "u", Pass: "hunter3"}}
	out := Redacted(&h)
	if strings.Contains(out, "hunter2") || strings.Contains(out, "hunter3") {
		t.Errorf("Redacted walks by type, so visibility must not decide whether a credential leaks: %s", out)
	}
	if !strings.Contains(out, "Public=p") || !strings.Contains(out, "Inner.User=u") {
		t.Errorf("Redacted must still show the non-secret fields by path: %s", out)
	}
}
