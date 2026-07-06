package rss

import (
	"strings"
	"testing"
	"time"
)

const rssDoc = `<?xml version="1.0"?>
<rss version="2.0">
  <channel>
    <title>Tweakers</title>
    <description>Nieuws &amp; meuk</description>
    <link>https://tweakers.net</link>
    <item>
      <title>Eerste item</title>
      <link>https://tweakers.net/1</link>
      <guid>guid-1</guid>
      <description>Beschrijving &amp;#8211; met entiteiten</description>
      <pubDate>Wed, 09 Feb 2011 01:59:11 +0200</pubDate>
    </item>
    <item>
      <title>Zonder guid</title>
      <link>https://tweakers.net/2</link>
      <description>size: 4.7 GB release</description>
      <pubDate>2012-03-28T13:10:00+00:00</pubDate>
    </item>
  </channel>
</rss>`

const atomDoc = `<?xml version="1.0"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Blogje</title>
  <updated>2026-07-01T10:00:00Z</updated>
  <link href="https://blog.example"/>
  <entry>
    <title>Atoompje</title>
    <link href="https://blog.example/post"/>
    <id>tag:blog,1</id>
    <published>2003-12-13T18:30:02Z</published>
    <content>Inhoud hier</content>
  </entry>
</feed>`

func TestParseRSS(t *testing.T) {
	f, items, err := parseFeed([]byte(rssDoc))
	if err != nil {
		t.Fatal(err)
	}
	if f.Title != "Tweakers" || f.Description != "Nieuws & meuk" || f.Link != "https://tweakers.net" {
		t.Fatalf("feed head = %+v", f)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d", len(items))
	}
	if items[0].Guid != "guid-1" || items[0].Title != "Eerste item" {
		t.Fatalf("item 0 = %+v", items[0])
	}
	if items[0].Description != "Beschrijving - met entiteiten" {
		t.Fatalf("decode: %q", items[0].Description)
	}
	want := time.Date(2011, 2, 9, 1, 59, 11, 0, time.FixedZone("", 2*3600))
	if !items[0].Time.Equal(want) {
		t.Fatalf("pubDate = %v, want %v", items[0].Time, want)
	}
	// missing guid falls back to title+link
	if items[1].Guid != "Zonder guidhttps://tweakers.net/2" {
		t.Fatalf("guid fallback = %q", items[1].Guid)
	}
	if !items[1].Time.Equal(time.Date(2012, 3, 28, 13, 10, 0, 0, time.UTC)) {
		t.Fatalf("iso pubDate = %v", items[1].Time)
	}
}

func TestParseAtom(t *testing.T) {
	f, items, err := parseFeed([]byte(atomDoc))
	if err != nil {
		t.Fatal(err)
	}
	if f.Title != "Blogje" || f.Link != "https://blog.example" {
		t.Fatalf("feed head = %+v", f)
	}
	if !strings.HasPrefix(f.Description, "Last update: ") {
		t.Fatalf("atom description = %q", f.Description)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d", len(items))
	}
	it := items[0]
	if it.Guid != "tag:blog,1" || it.Link != "https://blog.example/post" || it.Description != "Inhoud hier" {
		t.Fatalf("item = %+v", it)
	}
	if !it.Time.Equal(time.Date(2003, 12, 13, 18, 30, 2, 0, time.UTC)) {
		t.Fatalf("published = %v", it.Time)
	}
}

func TestParseNakedAmpersand(t *testing.T) {
	doc := strings.Replace(rssDoc, "Nieuws &amp; meuk", "Nieuws & meuk", 1)
	f, _, err := parseFeed([]byte(doc))
	if err != nil {
		t.Fatalf("naked & not repaired: %v", err)
	}
	if f.Description != "Nieuws & meuk" {
		t.Fatalf("description = %q", f.Description)
	}
}

func TestParseGarbage(t *testing.T) {
	if _, _, err := parseFeed([]byte("this is no xml at all <<<")); err == nil {
		t.Fatal("garbage parsed without error")
	}
}

func TestDecode(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"a &amp; b", "a & b"},
		{"x&#8230;", "x..."},
		{"kijk <a href=\"http://x\">hier</a> dan", "kijk hier dan"},
		{"regel1\nregel2   met    spaties", "regel1 regel2 met spaties"},
		{"voor<br/>na", "voor na"},
		{"<p>alinea</p> en <div>blok</div>", "alinea en blok"},
		{"<img src=x> kapot", " kapot"},
		{"rest<span>je</span>", "restje"},
	} {
		if got := decode(tc.in); got != tc.want {
			t.Errorf("decode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestColorquotes(t *testing.T) {
	got := colorquotes(`hij zei "hallo" en toen "doei"`)
	if got != "hij zei {y}hallo{/} en toen {g}doei{/}" {
		t.Fatalf("colorquotes = %q", got)
	}
}
