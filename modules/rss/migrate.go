package rss

import "fmt"

// MigrateFromPerl converts the Perl IRC_RSS storage (feeds + maxItemId,
// with DateTime item timestamps already unblessed to epochs by
// dump.pl) into the Go storage value. Returns the value to store under
// (namespace "rss", key "feeds") and the feed count.
func MigrateFromPerl(dump map[string]any) (*stored, int, error) {
	feedsAny, ok := dump["feeds"].(map[string]any)
	if !ok {
		return nil, 0, fmt.Errorf("rss: dump has no feeds key")
	}
	out := &stored{Feeds: make(map[string]*feed), MaxID: migInt(dump["maxItemId"])}
	for url, fvAny := range feedsAny {
		fv, ok := fvAny.(map[string]any)
		if !ok {
			return nil, 0, fmt.Errorf("rss: feed %q is %T", url, fvAny)
		}
		f := &feed{
			Tag:           migStr(fv["tag"]),
			Grep:          migStr(fv["grep"]),
			Title:         migStr(fv["title"]),
			Description:   migStr(fv["description"]),
			Link:          migStr(fv["link"]),
			LastUpdate:    int64(migInt(fv["lastupdate"])),
			History:       make(map[string]*item),
			Subscriptions: make(map[string]map[string]*subscription),
		}
		if fetcher, ok := fv["fetcher"].(map[string]any); ok {
			f.Refresh = migInt(fetcher["refresh"])
		}
		if f.Refresh == 0 {
			f.Refresh = defaultRefresh
		}
		if hist, ok := fv["history"].(map[string]any); ok {
			for guid, ivAny := range hist {
				iv, ok := ivAny.(map[string]any)
				if !ok {
					return nil, 0, fmt.Errorf("rss: %s history %q is %T", url, guid, ivAny)
				}
				f.History[guid] = &item{
					ID:          migStr(iv["_id"]),
					Title:       migStr(iv["title"]),
					Link:        migStr(iv["link"]),
					Guid:        migStr(iv["guid"]),
					Description: migStr(iv["description"]),
					Time:        int64(migInt(iv["datetime"])),
					Updated:     int64(migInt(iv["updatetime"])),
				}
			}
		}
		if subs, ok := fv["subscriptions"].(map[string]any); ok {
			for server, byChanAny := range subs {
				byChan, ok := byChanAny.(map[string]any)
				if !ok {
					continue
				}
				f.Subscriptions[server] = make(map[string]*subscription)
				for channel, svAny := range byChan {
					sv, ok := svAny.(map[string]any)
					if !ok {
						continue
					}
					sub := &subscription{
						User:     migStr(sv["user"]),
						Received: make(map[string]int64),
					}
					if recv, ok := sv["received"].(map[string]any); ok {
						for g, epoch := range recv {
							sub.Received[g] = int64(migInt(epoch))
						}
					}
					f.Subscriptions[server][channel] = sub
				}
			}
		}
		out.Feeds[url] = f
	}
	return out, len(out.Feeds), nil
}

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

func migStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
