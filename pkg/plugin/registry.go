package plugin

import (
	"fmt"
	"slices"
	"sync"
)

// The in-process loader's registry (PLG-1): compiled-in drivers register
// themselves, the core looks them up by name.
var (
	registryMu sync.RWMutex
	registry   = map[string]Driver{}
)

// Register adds a driver; registering a name twice is a programming error.
func Register(d Driver) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, ok := registry[d.Name()]; ok {
		panic(fmt.Sprintf("plugin: driver %q registered twice", d.Name()))
	}
	registry[d.Name()] = d
}

// Lookup returns the driver registered under name.
func Lookup(name string) (Driver, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	d, ok := registry[name]
	return d, ok
}

// Names lists the registered drivers, sorted.
func Names() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}
