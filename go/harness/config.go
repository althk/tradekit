// Package harness holds the operational plumbing every trading process needs
// and none should write again: configuration with secrets that cannot leak,
// log attributes that render domain types readably, a decision journal, and
// notifications that cannot stall the trading loop.
//
// It owns no configuration schema. Four donors each had a loader doing the
// same four things — read a file, overlay environment variables, resolve
// secrets, validate — and what they had in common was much less than their
// size suggested. Only the overlay and secret mechanics are shared; the shape
// of a project's configuration stays in the project.
package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Secret is a string that does not appear in logs or error messages.
//
// An API secret reaching a log is a real incident, and it happens through %v
// on a config struct in a startup line — which every one of the donor projects
// has. Making Secret the type of every credential field makes the default fmt,
// slog and JSON paths safe; only Reveal returns the value.
type Secret string

// Redaction is what a Secret renders as everywhere but Reveal.
const Redaction = "<redacted>"

// String renders the redaction, so %v and %s on a containing struct are safe.
func (Secret) String() string { return Redaction }

// GoString renders the redaction, so %#v is safe too.
func (Secret) GoString() string { return `harness.Secret("` + Redaction + `")` }

// MarshalJSON renders the redaction, so a config serialised for a status
// endpoint or a debug dump cannot carry the credential.
func (Secret) MarshalJSON() ([]byte, error) { return json.Marshal(Redaction) }

// UnmarshalJSON reads the real value, so a secret can arrive through JSON as
// well as TOML.
func (s *Secret) UnmarshalJSON(raw []byte) error {
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	*s = Secret(v)
	return nil
}

// LogValue renders the redaction, so a Secret passed to slog directly is safe.
func (Secret) LogValue() slog.Value { return slog.StringValue(Redaction) }

// Reveal returns the actual value. It is the only way to read one, which makes
// every use site greppable.
func (s Secret) Reveal() string { return string(s) }

// Load decodes a TOML file into cfg, then applies environment overrides and
// validates required fields.
//
// cfg is a pointer to the caller's own struct; this function owns no schema.
//
// A field tagged `env:"KITE_API_KEY"` takes that variable's value when it is
// set and non-empty, after the file is read. An unset or empty variable leaves
// the file's value alone, so a deployment can override one field without
// restating the file. Strings, Secret, integers, unsigned integers, floats,
// booleans and time.Duration are supported; nested structs are walked.
//
// A field tagged `validate:"required"` must be non-zero after the overlay.
// Every missing field is reported in one error rather than the first: a
// deployment that has to restart four times to discover four missing
// variables is why people put credentials in the file instead.
func Load(path string, cfg any) error {
	v := reflect.ValueOf(cfg)
	if v.Kind() != reflect.Pointer || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("harness: Load needs a pointer to a struct, got %T", cfg)
	}
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return fmt.Errorf("harness: reading %s: %w", path, err)
	}
	return Overlay(cfg)
}

// Overlay applies environment overrides to an already-populated cfg and
// validates it. Load calls it; a caller that decodes its file some other way
// can call it directly.
func Overlay(cfg any) error {
	v := reflect.ValueOf(cfg)
	if v.Kind() != reflect.Pointer || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("harness: Overlay needs a pointer to a struct, got %T", cfg)
	}
	var missing []string
	if err := walk(v.Elem(), "", &missing); err != nil {
		return err
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("harness: missing required config: %s", strings.Join(missing, ", "))
	}
	return nil
}

// walk applies env overrides and collects missing required fields, naming them
// by their dotted path so the error reads as the file does.
func walk(v reflect.Value, prefix string, missing *[]string) error {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		fv := v.Field(i)
		name := prefix + fieldName(f)

		if fv.Kind() == reflect.Pointer && fv.Type().Elem().Kind() == reflect.Struct {
			if fv.IsNil() {
				continue
			}
			fv = fv.Elem()
		}
		if fv.Kind() == reflect.Struct && fv.Type() != durationType {
			if err := walk(fv, name+".", missing); err != nil {
				return err
			}
			continue
		}

		if env := f.Tag.Get("env"); env != "" {
			if raw := os.Getenv(env); raw != "" {
				if err := set(fv, raw); err != nil {
					return fmt.Errorf("harness: %s from $%s: %w", name, env, err)
				}
			}
		}
		if f.Tag.Get("validate") == "required" && fv.IsZero() {
			*missing = append(*missing, name)
		}
	}
	return nil
}

var durationType = reflect.TypeOf(time.Duration(0))

// fieldName is the TOML key when tagged, else the Go name, so the error names
// the key the operator sees in the file.
func fieldName(f reflect.StructField) string {
	if tag := f.Tag.Get("toml"); tag != "" {
		if name, _, _ := strings.Cut(tag, ","); name != "" && name != "-" {
			return name
		}
	}
	return f.Name
}

// set parses raw into a field of a supported kind.
func set(fv reflect.Value, raw string) error {
	if !fv.CanSet() {
		return errors.New("field is not settable")
	}
	switch {
	case fv.Type() == durationType:
		d, err := time.ParseDuration(raw)
		if err != nil {
			return err
		}
		fv.SetInt(int64(d))
	case fv.Kind() == reflect.String:
		fv.SetString(raw)
	case fv.Kind() >= reflect.Int && fv.Kind() <= reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return err
		}
		fv.SetInt(n)
	case fv.Kind() >= reflect.Uint && fv.Kind() <= reflect.Uint64:
		n, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return err
		}
		fv.SetUint(n)
	case fv.Kind() == reflect.Float32 || fv.Kind() == reflect.Float64:
		x, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return err
		}
		fv.SetFloat(x)
	case fv.Kind() == reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return err
		}
		fv.SetBool(b)
	default:
		return fmt.Errorf("unsupported field kind %s for an env override", fv.Kind())
	}
	return nil
}

// Redacted renders a config with every Secret masked, one "path=value" per
// field, for the one startup line that is genuinely useful.
//
// It is the only supported way to print a config. fmt's %v is safe for
// exported Secret fields because of the String method, but an unexported field
// bypasses methods and prints the raw string; this walks by type instead, so
// the field's visibility does not decide whether the credential leaks.
func Redacted(cfg any) string {
	v := reflect.ValueOf(cfg)
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return "<nil>"
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return fmt.Sprint(cfg)
	}
	var parts []string
	render(v, "", &parts)
	return strings.Join(parts, " ")
}

var secretType = reflect.TypeOf(Secret(""))

func render(v reflect.Value, prefix string, parts *[]string) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		fv := v.Field(i)
		name := prefix + fieldName(f)
		if fv.Kind() == reflect.Pointer {
			if fv.IsNil() {
				*parts = append(*parts, name+"=<nil>")
				continue
			}
			fv = fv.Elem()
		}
		switch {
		case fv.Type() == secretType:
			*parts = append(*parts, name+"="+Redaction)
		case fv.Type() == durationType:
			*parts = append(*parts, name+"="+time.Duration(fv.Int()).String())
		case fv.Kind() == reflect.Struct:
			render(fv, name+".", parts)
		default:
			*parts = append(*parts, fmt.Sprintf("%s=%v", name, fv))
		}
	}
}
