package ego

import "fmt"

// MigrateFromPerl converts the Perl IRC_Ego "ego" hash
// (server -> nick -> {egoCount, sentences, channel: {chan: {...}}})
// into the Go storage value. Returns the value to store under
// (namespace "ego", key "ego") and the migrated nick count.
func MigrateFromPerl(dump map[string]any) (map[string]map[string]*nickStats, int, error) {
	ego, ok := dump["ego"].(map[string]any)
	if !ok {
		return nil, 0, fmt.Errorf("ego: dump has no ego key")
	}
	out := make(map[string]map[string]*nickStats)
	count := 0
	for server, nicksAny := range ego {
		nicks, ok := nicksAny.(map[string]any)
		if !ok {
			return nil, 0, fmt.Errorf("ego: server %q is %T", server, nicksAny)
		}
		out[server] = make(map[string]*nickStats)
		for nick, statsAny := range nicks {
			st, ok := statsAny.(map[string]any)
			if !ok {
				return nil, 0, fmt.Errorf("ego: %s/%s is %T", server, nick, statsAny)
			}
			ns := &nickStats{
				EgoCount:  migInt(st["egoCount"]),
				Sentences: migInt(st["sentences"]),
				Channels:  make(map[string]*chanStats),
			}
			if channels, ok := st["channel"].(map[string]any); ok {
				for ch, cvAny := range channels {
					cv, ok := cvAny.(map[string]any)
					if !ok {
						return nil, 0, fmt.Errorf("ego: %s/%s/%s is %T", server, nick, ch, cvAny)
					}
					ns.Channels[ch] = &chanStats{
						EgoCount:  migInt(cv["egoCount"]),
						Sentences: migInt(cv["sentences"]),
					}
				}
			}
			out[server][nick] = ns
			count++
		}
	}
	return out, count, nil
}

// migInt reads a JSON number that may arrive as float64 or a string.
func migInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case string:
		var i int
		fmt.Sscanf(n, "%d", &i)
		return i
	}
	return 0
}
