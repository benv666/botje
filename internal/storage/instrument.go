package storage

import "time"

// Instrument wraps a Store and reports every operation's duration to
// observe (op is get/put/delete/names; Close is not measured). Metrics
// food: observe must be goroutine-safe, operations can come from any
// goroutine.
func Instrument(s Store, observe func(op, ns string, seconds float64)) Store {
	return &instrumented{s: s, observe: observe}
}

type instrumented struct {
	s       Store
	observe func(op, ns string, seconds float64)
}

func (i *instrumented) time(op, ns string) func() {
	start := time.Now()
	return func() { i.observe(op, ns, time.Since(start).Seconds()) }
}

func (i *instrumented) Get(ns, name string, dst any) (bool, error) {
	defer i.time("get", ns)()
	return i.s.Get(ns, name, dst)
}

func (i *instrumented) Put(ns, name string, v any) error {
	defer i.time("put", ns)()
	return i.s.Put(ns, name, v)
}

func (i *instrumented) Delete(ns, name string) error {
	defer i.time("delete", ns)()
	return i.s.Delete(ns, name)
}

func (i *instrumented) Names(ns string) ([]string, error) {
	defer i.time("names", ns)()
	return i.s.Names(ns)
}

func (i *instrumented) Close() error { return i.s.Close() }
