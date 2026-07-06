package ego

import (
	"encoding/json"
	"strings"
	"testing"

	"go-botje/internal/bus"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/module"
	"go-botje/internal/storage"
)

const egoFixture = `{"ego":{"junerules":{
  "atlantes":{"egoCount":53,"sentences":88,"channel":{"#bvs":{"egoCount":53,"sentences":88}}},
  "baard":{"egoCount":1,"sentences":24,"channel":{"#kutkanker":{"sentences":10},"baard":{"egoCount":1,"sentences":14}}}
}}}`

func TestMigrateFromPerl(t *testing.T) {
	var dump map[string]any
	if err := json.Unmarshal([]byte(egoFixture), &dump); err != nil {
		t.Fatal(err)
	}
	out, count, err := MigrateFromPerl(dump)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}
	atl := out["junerules"]["atlantes"]
	if atl.EgoCount != 53 || atl.Sentences != 88 || atl.Channels["#bvs"].EgoCount != 53 {
		t.Fatalf("atlantes = %+v", atl)
	}
	// missing egoCount defaults to 0
	if out["junerules"]["baard"].Channels["#kutkanker"].EgoCount != 0 ||
		out["junerules"]["baard"].Channels["#kutkanker"].Sentences != 10 {
		t.Fatalf("baard #kutkanker = %+v", out["junerules"]["baard"].Channels["#kutkanker"])
	}
}

// the migrated value must be readable by a fresh module: !ego reports
// the imported counts.
func TestMigratedReadableByModule(t *testing.T) {
	var dump map[string]any
	json.Unmarshal([]byte(egoFixture), &dump)
	out, _, err := MigrateFromPerl(dump)
	if err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemory()
	if err := store.Put("ego", "ego", out); err != nil {
		t.Fatal(err)
	}

	b := bus.New()
	b.RegisterEvent("IRC_PRIVMSG")
	cmds := cmd.New()
	var sent []string
	m := New()
	if err := m.Load(&module.Context{
		Bus: b, Cmd: cmds, Store: store,
		Privmsg: func(ch, msg string) { sent = append(sent, msg) },
	}); err != nil {
		t.Fatal(err)
	}
	ev := &bus.Event{Name: "IRC_PRIVMSG", Server: "junerules", Channel: "#bvs",
		Msg: "!ego atlantes", Extra: map[string]any{}}
	ev.Sender.Nick = "Someone"
	b.Submit(ev)
	cmds.Handle(ev)
	if len(sent) != 1 || !strings.Contains(sent[0], "{r}53{/}x in {r}88{/}") {
		t.Fatalf("migrated !ego = %q", sent)
	}
}
