package steam

import "testing"

// searchFragment is the shape store.steampowered.com returns: an HTML fragment
// inside JSON, so quotes and closing slashes are backslash-escaped.
const searchFragment = `{"success":1,"results_html":"<a href=\"https:\/\/store.steampowered.com\/app\/4165890\/Steam_Frame\/\" data-ds-appid=\"4165890\" data-ds-itemkey=\"App_4165890\" class=\"search_result_row\">\n<div class=\"responsive_search_name_combined\">\n<span class=\"title\">Steam Frame<\/span>\n<\/div><\/a>\n<a href=\"x\" data-ds-appid=\"1142710\" class=\"search_result_row\">\n<span class=\"title\">Total War: WARHAMMER III<\/span>\n<\/a>\n<a href=\"x\" data-ds-appid=\"730\" class=\"search_result_row\">\n<span class=\"title\">Counter-Strike 2 &amp; Friends<\/span>\n<\/a>"}`

func TestRowReadsTitles(t *testing.T) {
	matches := rowRe.FindAllStringSubmatch(searchFragment, -1)
	if len(matches) != 3 {
		t.Fatalf("got %d rows, want 3", len(matches))
	}

	want := []struct{ id, title string }{
		{"4165890", "Steam Frame"},
		{"1142710", "Total War: WARHAMMER III"},
		{"730", "Counter-Strike 2 &amp; Friends"},
	}
	for i, w := range want {
		if matches[i][1] != w.id {
			t.Errorf("row %d: appid = %q, want %q", i, matches[i][1], w.id)
		}
		if matches[i][2] != w.title {
			t.Errorf("row %d: title = %q, want %q", i, matches[i][2], w.title)
		}
	}
}

func TestAppidRegexStillMatches(t *testing.T) {
	ids := appidRe.FindAllStringSubmatch(searchFragment, -1)
	if len(ids) != 3 {
		t.Fatalf("got %d app ids, want 3", len(ids))
	}
}

func TestAppidFromURL(t *testing.T) {
	cases := map[string]int{
		"https://cdn.cloudflare.steamstatic.com/steam/apps/1091500/library_600x900_2x.jpg": 1091500,
		"https://cdn.cloudflare.steamstatic.com/steam/apps/730/library_600x900.jpg":        730,
		"https://example.com/not-steam.jpg":                                                0,
		"":                                                                                 0,
	}
	for u, want := range cases {
		if got := appidFromURL(u); got != want {
			t.Errorf("appidFromURL(%q) = %d, want %d", u, got, want)
		}
	}
}

func TestNameCache(t *testing.T) {
	c := newNameCache()
	if got := c.get(730); got != "" {
		t.Errorf("empty cache returned %q", got)
	}
	c.set(730, "Counter-Strike 2")
	if got := c.get(730); got != "Counter-Strike 2" {
		t.Errorf("get = %q", got)
	}
	c.set(570, "")
	if got := c.get(570); got != "" {
		t.Error("an empty name was cached")
	}
}
