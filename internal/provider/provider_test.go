package provider

import "testing"

func TestCaption(t *testing.T) {
	cases := []struct {
		name  string
		image Image
		want  string
	}{
		{
			name:  "tmdb movie",
			image: Image{Title: "Dune: Part Two", Date: "2024-03-01"},
			want:  "Dune: Part Two (2024)",
		},
		{
			name:  "itunes album",
			image: Image{Title: "Kind of Blue", Creator: "Miles Davis", Date: "1959-08-17T07:00:00Z"},
			want:  "Kind of Blue - Miles Davis (1959)",
		},
		{
			name:  "artic painting",
			image: Image{Title: "Water Lilies", Creator: "Claude Monet", Date: "c. 1906"},
			want:  "Water Lilies - Claude Monet (1906)",
		},
		{
			name:  "wikidata inception",
			image: Image{Title: "Mona Lisa", Creator: "Leonardo da Vinci", Date: "1503-01-01T00:00:00Z"},
			want:  "Mona Lisa - Leonardo da Vinci (1503)",
		},
		{
			name:  "steam name only",
			image: Image{Title: "Hades"},
			want:  "Hades",
		},
		{
			name:  "unparseable date is left out",
			image: Image{Title: "Untitled", Date: "n.d."},
			want:  "Untitled",
		},
		{
			name:  "creator without title",
			image: Image{Creator: "Jane Doe", Date: "2019-06-09T12:00:00-04:00"},
			want:  "Jane Doe (2019)",
		},
		{
			name:  "nothing known",
			image: Image{URL: "https://example.com/x.jpg"},
			want:  "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.image.Caption(); got != tc.want {
				t.Errorf("Caption() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParsed(t *testing.T) {
	cases := []struct {
		date string
		year int
		ok   bool
	}{
		{"2024-03-01", 2024, true},
		{"1959-08-17T07:00:00Z", 1959, true},
		{"2019-06-09T12:00:00-04:00", 2019, true},
		{"1906", 1906, true},
		{"c. 1906", 1906, true},
		{"1890–1891", 1890, true},
		{"", 0, false},
		{"n.d.", 0, false},
		{"unknown", 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.date, func(t *testing.T) {
			got, ok := Image{Date: tc.date}.Parsed()
			if ok != tc.ok {
				t.Fatalf("Parsed(%q) ok = %v, want %v", tc.date, ok, tc.ok)
			}
			if ok && got.Year() != tc.year {
				t.Errorf("Parsed(%q) year = %d, want %d", tc.date, got.Year(), tc.year)
			}
		})
	}
}

func TestURLs(t *testing.T) {
	images := []Image{{URL: "a"}, {URL: "b"}}
	got := URLs(images)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("URLs() = %v", got)
	}
}
