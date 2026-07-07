package ticker

import "fmt"

// MigrateStats reports what a Perl import contained.
type MigrateStats struct {
	Tickers       int
	Subscriptions int
	Points        int
}

// MigrateFromPerl converts the Perl IRC_Ticker storage into the Go
// storage values (namespace "ticker", keys "tickers" and
// "tickerdata"). Alarm values are strings in Perl and numbers here;
// the per-ticker fetcher subtree is runtime state and is dropped.
// Data points pass through raw (they are the stored API responses,
// same shape both sides), except points without a "time" key: those
// would sort as epoch 0, so they are dropped.
func MigrateFromPerl(dump map[string]any) (*stored, MigrateStats, error) {
	var st MigrateStats
	out := &stored{Tickers: map[string]*tickerSub{}, Data: map[string][]sample{}}

	if tickersAny, ok := dump["tickers"]; ok {
		tickers, ok := tickersAny.(map[string]any)
		if !ok {
			return nil, st, fmt.Errorf("ticker: tickers is %T", tickersAny)
		}
		for sym, tvAny := range tickers {
			tv, ok := tvAny.(map[string]any)
			if !ok {
				return nil, st, fmt.Errorf("ticker: %s is %T", sym, tvAny)
			}
			t := &tickerSub{Subscriptions: map[string]map[string]map[string]*subInfo{}}
			if subs, ok := tv["subscriptions"].(map[string]any); ok {
				for server, byChanAny := range subs {
					byChan, ok := byChanAny.(map[string]any)
					if !ok {
						continue
					}
					t.Subscriptions[server] = map[string]map[string]*subInfo{}
					for channel, byUserAny := range byChan {
						byUser, ok := byUserAny.(map[string]any)
						if !ok {
							continue
						}
						t.Subscriptions[server][channel] = map[string]*subInfo{}
						for user, svAny := range byUser {
							si := &subInfo{}
							if sv, ok := svAny.(map[string]any); ok {
								if alarms, ok := sv["alarms"].(map[string]any); ok {
									si.Alarms = map[string]float64{}
									for name, val := range alarms {
										si.Alarms[name] = num(val)
									}
								}
							}
							t.Subscriptions[server][channel][user] = si
							st.Subscriptions++
						}
					}
				}
			}
			out.Tickers[sym] = t
			st.Tickers++
		}
	}

	if dataAny, ok := dump["tickerdata"]; ok {
		data, ok := dataAny.(map[string]any)
		if !ok {
			return nil, st, fmt.Errorf("ticker: tickerdata is %T", dataAny)
		}
		for sym, pointsAny := range data {
			points, ok := pointsAny.([]any)
			if !ok {
				return nil, st, fmt.Errorf("ticker: tickerdata %s is %T", sym, pointsAny)
			}
			kept := make([]sample, 0, len(points))
			for _, pAny := range points {
				p, ok := pAny.(map[string]any)
				if !ok {
					continue
				}
				if _, ok := p["time"]; !ok {
					continue
				}
				kept = append(kept, sample(p))
			}
			out.Data[sym] = kept
			st.Points += len(kept)
		}
	}

	return out, st, nil
}
