package main

import (
	"fmt"

	"go-botje/modules/karma"
)

// karmaStats is the verification summary for a karma migration.
type karmaStats struct {
	GlobalItems  int
	Servers      int
	Channels     int
	ChannelItems int
}

// karmaFromPerl maps the Perl karma hash (global entries mixed into
// the server level under __GLOBAL_IRC_Karma__) onto the Go layout.
func karmaFromPerl(dump map[string]any) (karma.Data, karmaStats, error) {
	data := karma.Data{
		Servers: make(map[string]map[string]map[string]*karma.Entry),
		Global:  make(map[string]*karma.Entry),
	}
	var stats karmaStats

	top, ok := dump["karma"].(map[string]any)
	if !ok {
		return data, stats, fmt.Errorf("karma: dump has no karma key")
	}
	for server, v := range top {
		if server == "__GLOBAL_IRC_Karma__" {
			items, ok := v.(map[string]any)
			if !ok {
				return data, stats, fmt.Errorf("karma: global level is %T", v)
			}
			for item, e := range items {
				entry, err := entryFromPerl(e)
				if err != nil {
					return data, stats, fmt.Errorf("karma: global %q: %w", item, err)
				}
				data.Global[item] = entry
			}
			stats.GlobalItems = len(data.Global)
			continue
		}
		channels, ok := v.(map[string]any)
		if !ok {
			return data, stats, fmt.Errorf("karma: server %q is %T", server, v)
		}
		stats.Servers++
		data.Servers[server] = make(map[string]map[string]*karma.Entry)
		for channel, cv := range channels {
			items, ok := cv.(map[string]any)
			if !ok {
				return data, stats, fmt.Errorf("karma: %s/%s is %T", server, channel, cv)
			}
			stats.Channels++
			data.Servers[server][channel] = make(map[string]*karma.Entry)
			for item, e := range items {
				entry, err := entryFromPerl(e)
				if err != nil {
					return data, stats, fmt.Errorf("karma: %s/%s/%q: %w", server, channel, item, err)
				}
				data.Servers[server][channel][item] = entry
				stats.ChannelItems++
			}
		}
	}
	return data, stats, nil
}

func entryFromPerl(v any) (*karma.Entry, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("entry is %T", v)
	}
	e := &karma.Entry{}
	if k, ok := toInt(m["karma"]); ok {
		e.Karma = k
	}
	if l, ok := toInt(m["last"]); ok {
		e.Last = int64(l)
	}
	if reasons, ok := m["reason"].(map[string]any); ok {
		e.Reason = make(map[string]map[string]int)
		for ud, rv := range reasons {
			counts, ok := rv.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("reason %q is %T", ud, rv)
			}
			e.Reason[ud] = make(map[string]int)
			for reason, cv := range counts {
				if c, ok := toInt(cv); ok {
					e.Reason[ud][reason] = c
				}
			}
		}
	}
	return e, nil
}

// toInt handles json numbers and the strings Perl sometimes stores
// numbers as.
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case string:
		var i int
		if _, err := fmt.Sscanf(n, "%d", &i); err == nil {
			return i, true
		}
	}
	return 0, false
}
