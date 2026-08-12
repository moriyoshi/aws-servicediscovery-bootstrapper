package main

import (
	"fmt"
	"time"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"

	"github.com/moriyoshi/muster/internal/provider"
)

// entryToStarlark converts a discovered instance into a Starlark struct with
// .ipv4 / .ipv6 / .port fields.
func entryToStarlark(e provider.Instance) starlark.Value {
	return starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
		"ipv4": starlark.String(e.IPv4Addr),
		"ipv6": starlark.String(e.IPv6Addr),
		"port": starlark.MakeInt(e.Port),
	})
}

func entriesToStarlark(es []provider.Instance) *starlark.List {
	vals := make([]starlark.Value, len(es))
	for i, e := range es {
		vals[i] = entryToStarlark(e)
	}
	return starlark.NewList(vals)
}

// selfToStarlark exposes the instance identity as the SELF global struct.
//
// Absent identity stays None rather than becoming a struct of empty strings: a
// starlarkstruct is always truthy, so an `if SELF:` guard would silently become
// dead code.
//
// Provider-specific fields hang off a sub-struct named for the provider
// (SELF.aws.vpc_id), so reading one on a build for another cloud raises "struct
// has no .aws attribute" instead of returning something plausible. Anything
// under that name is a visible declaration that the script is not portable.
func selfToStarlark(m *provider.Identity) starlark.Value {
	if m == nil {
		return starlark.None
	}
	fields := starlark.StringDict{
		"id":         starlark.String(m.ID),
		"name":       starlark.String(m.Name),
		"group":      starlark.String(m.Group),
		"service":    starlark.String(m.Service),
		"zone":       starlark.String(m.Zone),
		"region":     starlark.String(m.Region),
		"network":    starlark.String(m.Network),
		"ipv4":       starlark.String(m.IPv4),
		"ipv6":       starlark.String(m.IPv6),
		"created_at": starlark.String(m.CreatedAt),
	}
	if len(m.Extra) > 0 && m.Provider != "" {
		extra := make(starlark.StringDict, len(m.Extra))
		for k, v := range m.Extra {
			extra[k] = starlark.String(v)
		}
		fields[m.Provider] = starlarkstruct.FromStringDict(starlarkstruct.Default, extra)
	}
	return starlarkstruct.FromStringDict(starlarkstruct.Default, fields)
}

func stringsToStarlark(ss []string) *starlark.List {
	vals := make([]starlark.Value, len(ss))
	for i, s := range ss {
		vals[i] = starlark.String(s)
	}
	return starlark.NewList(vals)
}

// unpackDuration interprets a Starlark value as a duration: None => 0, a number
// is seconds, and a string is parsed via time.ParseDuration ("5s", "500ms").
func unpackDuration(v starlark.Value) (time.Duration, error) {
	switch t := v.(type) {
	case nil, starlark.NoneType:
		return 0, nil
	case starlark.String:
		d, err := time.ParseDuration(string(t))
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: %w", string(t), err)
		}
		return d, nil
	case starlark.Int:
		i, _ := t.Int64()
		return time.Duration(i) * time.Second, nil
	case starlark.Float:
		return time.Duration(float64(t) * float64(time.Second)), nil
	default:
		return 0, fmt.Errorf("expected duration (number of seconds or string), got %s", v.Type())
	}
}

// unpackPollInterval interprets an optional interval argument, defaulting to 1s
// and rejecting non-positive values.
func unpackPollInterval(v starlark.Value) (time.Duration, error) {
	if v == nil || v == starlark.None {
		return time.Second, nil
	}
	d, err := unpackDuration(v)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("interval must be positive")
	}
	return d, nil
}

func starlarkAsInt(v starlark.Value) (int, error) {
	i, ok := v.(starlark.Int)
	if !ok {
		return 0, fmt.Errorf("expected int, got %s", v.Type())
	}
	n, _ := i.Int64()
	return int(n), nil
}

func starlarkAsFloat(v starlark.Value) (float64, error) {
	switch x := v.(type) {
	case starlark.Float:
		return float64(x), nil
	case starlark.Int:
		n, _ := x.Int64()
		return float64(n), nil
	default:
		return 0, fmt.Errorf("expected number, got %s", v.Type())
	}
}

// starlarkToStrings converts a Starlark list/tuple of strings to a []string.
func starlarkToStrings(v starlark.Value) ([]string, error) {
	iter := starlark.Iterate(v)
	if iter == nil {
		return nil, fmt.Errorf("expected an iterable of strings, got %s", v.Type())
	}
	defer iter.Done()
	var out []string
	var x starlark.Value
	for iter.Next(&x) {
		s, ok := starlark.AsString(x)
		if !ok {
			return nil, fmt.Errorf("expected string element, got %s", x.Type())
		}
		out = append(out, s)
	}
	return out, nil
}

// starlarkDictToEnv converts a Starlark dict {name: value} into a slice of
// "NAME=VALUE" strings. Values are coerced to strings.
func starlarkDictToEnv(v starlark.Value) ([]string, error) {
	d, ok := v.(*starlark.Dict)
	if !ok {
		return nil, fmt.Errorf("env must be a dict, got %s", v.Type())
	}
	var out []string
	for _, item := range d.Items() {
		name, ok := starlark.AsString(item[0])
		if !ok {
			return nil, fmt.Errorf("env key must be a string, got %s", item[0].Type())
		}
		val, ok := starlark.AsString(item[1])
		if !ok {
			return nil, fmt.Errorf("env value for %q must be a string, got %s", name, item[1].Type())
		}
		out = append(out, name+"="+val)
	}
	return out, nil
}
