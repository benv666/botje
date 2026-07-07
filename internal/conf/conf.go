// Package conf is the typed settings registry, the Go counterpart of the
// Perl Conf module. Modules create typed settings (int/float/string/bool)
// with defaults; stored values are applied when a setting is created; the
// read-only file config wins over stored values; a validated Set fires
// OnChange so the owner can emit config_changed on the bus. Dispatcher
// goroutine only, like the rest of the module-facing surface.
package conf

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
)

type kind int

const (
	kindInt kind = iota
	kindFloat
	kindString
	kindBool
)

func (k kind) String() string {
	return [...]string{"int", "float", "string", "bool"}[k]
}

type setting struct {
	kind kind
	raw  string // current value, string form (persisted form)
}

// Conf holds registered settings and pending stored/override values.
type Conf struct {
	// OnChange, when set, is called with the setting name after every
	// successful Set. The owner wires this to a config_changed event.
	OnChange func(name string)

	settings  map[string]*setting
	stored    map[string]string // persisted values, applied at create
	overrides map[string]string // file config, always wins on read
}

// New returns an empty settings registry.
func New() *Conf {
	return &Conf{
		settings:  make(map[string]*setting),
		stored:    make(map[string]string),
		overrides: make(map[string]string),
	}
}

// LoadStored supplies values persisted by a previous run. They take
// effect when the matching setting is created.
func (c *Conf) LoadStored(values map[string]string) {
	maps.Copy(c.stored, values)
}

// SetFileOverrides supplies read-only file config values. An overridden
// setting always reads the file value, whatever is stored or set.
func (c *Conf) SetFileOverrides(values map[string]string) {
	maps.Copy(c.overrides, values)
}

// create registers a setting unless it already exists, applying a valid
// stored value over the default.
func (c *Conf) create(name string, k kind, def string) {
	if _, ok := c.settings[name]; ok {
		return // module reload re-creates its settings; keep current value
	}
	raw := def
	if stored, ok := c.stored[name]; ok && parseOK(k, stored) {
		raw = stored
	}
	c.settings[name] = &setting{kind: k, raw: raw}
}

// CreateInt registers an int setting. Re-creating an existing setting
// keeps its current value.
func (c *Conf) CreateInt(name string, def int) {
	c.create(name, kindInt, strconv.Itoa(def))
}

// CreateFloat registers a float setting.
func (c *Conf) CreateFloat(name string, def float64) {
	c.create(name, kindFloat, strconv.FormatFloat(def, 'g', -1, 64))
}

// CreateString registers a string setting.
func (c *Conf) CreateString(name, def string) {
	c.create(name, kindString, def)
}

// CreateBool registers a bool setting.
func (c *Conf) CreateBool(name string, def bool) {
	c.create(name, kindBool, strconv.FormatBool(def))
}

func parseOK(k kind, raw string) bool {
	var err error
	switch k {
	case kindInt:
		_, err = strconv.Atoi(raw)
	case kindFloat:
		_, err = strconv.ParseFloat(raw, 64)
	case kindBool:
		_, err = strconv.ParseBool(raw)
	case kindString:
	}
	return err == nil
}

// Set parses and validates raw for the setting's type, updates the
// stored value, and fires OnChange. Errors on unknown names and values
// that do not parse; the value is then unchanged and OnChange stays quiet.
func (c *Conf) Set(name, raw string) error {
	s, ok := c.settings[name]
	if !ok {
		return fmt.Errorf("conf: no such setting %q", name)
	}
	if !parseOK(s.kind, raw) {
		return fmt.Errorf("conf: %q is not a valid %s for %s", raw, s.kind, name)
	}
	s.raw = raw
	c.stored[name] = raw
	if c.OnChange != nil {
		c.OnChange(name)
	}
	return nil
}

// Stored returns the values a next run should feed to LoadStored:
// everything explicitly Set plus loaded values whose setting has not
// been created (yet) this run. Defaults are never included, so a
// changed default applies to installations that never touched the
// setting.
func (c *Conf) Stored() map[string]string {
	return maps.Clone(c.stored)
}

// read returns the effective raw value (file override wins) after
// checking the type, panicking on misuse: a module reading a setting it
// did not create correctly is a programming error.
func (c *Conf) read(name string, k kind) string {
	s, ok := c.settings[name]
	if !ok {
		panic(fmt.Sprintf("conf: setting %q does not exist", name))
	}
	if s.kind != k {
		panic(fmt.Sprintf("conf: setting %q is %s, read as %s", name, s.kind, k))
	}
	if ov, ok := c.overrides[name]; ok && parseOK(k, ov) {
		return ov
	}
	return s.raw
}

// Int reads an int setting. Panics if the setting does not exist or has
// a different type: that is a programming error in the calling module.
func (c *Conf) Int(name string) int {
	v, _ := strconv.Atoi(c.read(name, kindInt))
	return v
}

// Float reads a float setting.
func (c *Conf) Float(name string) float64 {
	v, _ := strconv.ParseFloat(c.read(name, kindFloat), 64)
	return v
}

// String reads a string setting.
func (c *Conf) String(name string) string {
	return c.read(name, kindString)
}

// Bool reads a bool setting.
func (c *Conf) Bool(name string) bool {
	v, _ := strconv.ParseBool(c.read(name, kindBool))
	return v
}

// Dump returns all current values as strings for persistence. File
// overrides are not dumped; the persisted value is the set/stored one.
func (c *Conf) Dump() map[string]string {
	out := make(map[string]string, len(c.settings))
	for name, s := range c.settings {
		out[name] = s.raw
	}
	return out
}

// List returns all registered setting names, sorted.
func (c *Conf) List() []string {
	out := make([]string, 0, len(c.settings))
	for name := range c.settings {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}
