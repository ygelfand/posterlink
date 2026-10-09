package wikidata

import "testing"

const filePath = "http://commons.wikimedia.org/wiki/Special:FilePath/"

func TestImageURL(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		width int
		want  string
	}{
		{
			name:  "snaps up to a stored bucket",
			raw:   filePath + "Van%20Gogh%20-%20Starry%20Night%20-%20Google%20Art%20Project.jpg",
			width: 1000,
			want: "https://upload.wikimedia.org/wikipedia/commons/thumb/e/ea/" +
				"Van_Gogh_-_Starry_Night_-_Google_Art_Project.jpg/" +
				"1280px-Van_Gogh_-_Starry_Night_-_Google_Art_Project.jpg",
		},
		{
			name:  "exact bucket is left alone",
			raw:   filePath + "Van%20Gogh%20-%20Starry%20Night%20-%20Google%20Art%20Project.jpg",
			width: 960,
			want: "https://upload.wikimedia.org/wikipedia/commons/thumb/e/ea/" +
				"Van_Gogh_-_Starry_Night_-_Google_Art_Project.jpg/" +
				"960px-Van_Gogh_-_Starry_Night_-_Google_Art_Project.jpg",
		},
		{
			name:  "zero width serves the original",
			raw:   filePath + "Van%20Gogh%20-%20Starry%20Night%20-%20Google%20Art%20Project.jpg",
			width: 0,
			want: "https://upload.wikimedia.org/wikipedia/commons/e/ea/" +
				"Van_Gogh_-_Starry_Night_-_Google_Art_Project.jpg",
		},
		{
			name:  "above the largest bucket serves the original",
			raw:   filePath + "Van%20Gogh%20-%20Starry%20Night%20-%20Google%20Art%20Project.jpg",
			width: 5000,
			want: "https://upload.wikimedia.org/wikipedia/commons/e/ea/" +
				"Van_Gogh_-_Starry_Night_-_Google_Art_Project.jpg",
		},
		{
			name:  "non-ascii name hashes over the decoded bytes",
			raw:   filePath + "Caravaggio%20%E2%80%94%20Supper%20at%20Emmaus.jpg",
			width: 500,
			want: "https://upload.wikimedia.org/wikipedia/commons/thumb/7/73/" +
				"Caravaggio_%E2%80%94_Supper_at_Emmaus.jpg/" +
				"500px-Caravaggio_%E2%80%94_Supper_at_Emmaus.jpg",
		},
		{
			name:  "tiff renders to jpeg under a different leaf",
			raw:   filePath + "Example%20painting.tif",
			width: 960,
			want: "https://upload.wikimedia.org/wikipedia/commons/thumb/c/c5/" +
				"Example_painting.tif/lossy-page1-960px-Example_painting.tif.jpg",
		},
		{
			name:  "svg renders to png",
			raw:   filePath + "Example%20painting.svg",
			width: 960,
			want: "https://upload.wikimedia.org/wikipedia/commons/thumb/2/20/" +
				"Example_painting.svg/960px-Example_painting.svg.png",
		},
		{
			name:  "http is upgraded",
			raw:   "http://commons.wikimedia.org/wiki/Special:FilePath/A.jpg",
			width: 330,
			want:  "https://upload.wikimedia.org/wikipedia/commons/thumb/6/67/A.jpg/330px-A.jpg",
		},
		{
			name:  "empty stays empty",
			raw:   "",
			width: 500,
			want:  "",
		},
		{
			name:  "a url that is not a file path keeps the old form",
			raw:   "https://example.com/image.jpg",
			width: 500,
			want:  "https://example.com/image.jpg?width=500",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := imageURL(tc.raw, tc.width); got != tc.want {
				t.Errorf("imageURL(%q, %d) =\n  %q\nwant\n  %q", tc.raw, tc.width, got, tc.want)
			}
		})
	}
}

func TestBucket(t *testing.T) {
	cases := map[int]int{
		-1: 0, 0: 0, 1: 330, 330: 330, 331: 500, 500: 500,
		501: 960, 960: 960, 1000: 1280, 1300: 1920, 1920: 1920,
		2000: 3840, 3840: 3840, 3841: 0,
	}
	for in, want := range cases {
		if got := bucket(in); got != want {
			t.Errorf("bucket(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestCommonsDirMatchesCommons(t *testing.T) {
	cases := map[string]string{
		"Caravaggio_—_Supper_at_Emmaus.jpg":                "7/73",
		"Van_Gogh_-_Starry_Night_-_Google_Art_Project.jpg": "e/ea",
		"Madonna_di_Loreto-Caravaggio_(c.1604-6).jpg":      "5/57",
	}
	for name, want := range cases {
		if got := commonsDir(name); got != want {
			t.Errorf("commonsDir(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestCommonsFile(t *testing.T) {
	cases := map[string]string{
		filePath + "A%20B.jpg":                           "A_B.jpg",
		filePath + "A%20B.jpg?width=500":                 "A_B.jpg",
		filePath + "Caravaggio%20%E2%80%94%20Supper.jpg": "Caravaggio_—_Supper.jpg",
		"https://example.com/x.jpg":                      "",
		filePath:                                         "",
	}
	for raw, want := range cases {
		if got := commonsFile(raw); got != want {
			t.Errorf("commonsFile(%q) = %q, want %q", raw, got, want)
		}
	}
}
