package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
)

func readRaw(path string) (map[string]any, error) {
	raw := map[string]any{}
	body, err := os.ReadFile(path) //nolint:gosec // G304: fixed name inside our own data dir
	switch {
	case errors.Is(err, os.ErrNotExist):
		return raw, nil
	case err != nil:
		return nil, fmt.Errorf("config: read: %w", err)
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("config: %s: %w", File, err)
	}
	if raw == nil { // "null"
		raw = map[string]any{}
	}
	return raw, nil
}

// decode reads the known settings from raw on top of the defaults.
func decode(raw map[string]any) (Settings, error) {
	s := Defaults()
	b, err := json.Marshal(raw)
	if err != nil {
		return s, fmt.Errorf("config: encode: %w", err)
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return Defaults(), fmt.Errorf("config: %s: %w", File, err)
	}
	s.SchemaVersion = SchemaVersion
	s.normalize()
	return s, nil
}

// migrate upgrades an older layout in place; it reports whether anything changed.
func migrate(raw map[string]any) bool {
	v, _ := raw["schemaVersion"].(float64)
	if int(v) >= SchemaVersion {
		return false
	}
	if len(raw) == 0 { // no file: nothing to upgrade, written on the first change
		return false
	}
	section := func(name string) map[string]any {
		m, ok := raw[name].(map[string]any)
		if !ok {
			m = map[string]any{}
			raw[name] = m
		}
		return m
	}
	// v1 flat keys → sections (meaning kept).
	for _, k := range []string{"startWithWindows", "startMinimized"} {
		if val, ok := raw[k]; ok {
			section("general")[k] = val
			delete(raw, k)
		}
	}
	if val, ok := raw["pollIntervalMinutes"].(float64); ok {
		// "Sync every N minutes": new changes show up within N minutes. The
		// default (5) is today's balanced mode; anything else becomes custom
		// with N as the active-project poll interval.
		if int(val) > 0 && int(val) != 5 {
			sy := section("sync")
			sy["mode"] = SyncCustom
			provs, ok := sy["providers"].(map[string]any)
			if !ok {
				provs = map[string]any{}
				sy["providers"] = provs
			}
			gh, ok := provs["github"].(map[string]any)
			if !ok {
				gh = map[string]any{}
				provs["github"] = gh
			}
			gh["activeMinutes"] = val
			if val > float64(defaultGitHub().IdleMinutes) {
				gh["idleMinutes"] = val
			}
		}
		delete(raw, "pollIntervalMinutes")
	}
	raw["schemaVersion"] = SchemaVersion
	return true
}

// write stores s.cur (known keys, normalised) merged into s.raw (unknown keys
// kept): temp file + fsync + rename, the previous file kept as config.json.bak.
func (s *Store) write() error {
	known, err := toMap(s.cur)
	if err != nil {
		return err
	}
	out := deepCopy(s.raw)
	mergeKnown(out, known)
	s.raw = out
	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("config: encode: %w", err)
	}
	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return fmt.Errorf("config: dir: %w", err)
	}
	path := filepath.Join(s.dir, File)
	if err := copyFile(path, path+".bak"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("config: backup: %w", err)
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) //nolint:gosec // G304: fixed name inside our own data dir
	if err != nil {
		return fmt.Errorf("config: write: %w", err)
	}
	_, werr := f.Write(append(body, '\n'))
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("config: write: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("config: replace: %w", err)
	}
	return nil
}

func copyFile(from, to string) error {
	in, err := os.Open(from) //nolint:gosec // G304: our own config file
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) //nolint:gosec // G304: our own config file
	if err != nil {
		return err
	}
	_, cerr := io.Copy(out, in)
	return errors.Join(cerr, out.Close())
}

func toMap(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("config: encode: %w", err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("config: encode: %w", err)
	}
	return m, nil
}

// mergeKnown writes known into dst; nested objects merge so unknown nested keys stay.
func mergeKnown(dst, known map[string]any) {
	for k, v := range known {
		if km, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				mergeKnown(dm, km)
				continue
			}
		}
		dst[k] = v
	}
}

// mergePatch applies an RFC 7396 JSON merge patch: null deletes, objects merge,
// anything else replaces.
func mergePatch(dst, patch map[string]any) {
	for k, v := range patch {
		switch pv := v.(type) {
		case nil:
			delete(dst, k)
		case map[string]any:
			dm, ok := dst[k].(map[string]any)
			if !ok {
				dm = map[string]any{}
				dst[k] = dm
			}
			mergePatch(dm, pv)
		default:
			dst[k] = v
		}
	}
}

func deepCopy(m map[string]any) map[string]any {
	out := maps.Clone(m)
	for k, v := range out {
		switch t := v.(type) {
		case map[string]any:
			out[k] = deepCopy(t)
		case []any:
			out[k] = append([]any(nil), t...)
		}
	}
	return out
}
