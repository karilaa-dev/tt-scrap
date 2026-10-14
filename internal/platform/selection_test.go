package platform

import "testing"

func TestAppVideoSelectionRanksResolutionThenFPSAndBitrate(t *testing.T) {
	address := func(width, height int, fps float64, bitrate int, source string) object {
		return object{"width": width, "height": height, "fps": fps, "bit_rate": bitrate, "url_list": []any{source, source + "-backup"}}
	}
	for _, test := range []struct {
		name  string
		video object
		want  string
	}{
		{"resolution", object{
			"play_addr":      address(1080, 1920, 30, 1000, "full"),
			"play_addr_h264": address(720, 1280, 60, 9000, "small"),
		}, "full"},
		{"frame rate", object{
			"play_addr":      address(1080, 1920, 30, 9000, "slow"),
			"play_addr_h264": address(1080, 1920, 59.94, 1000, "smooth"),
		}, "smooth"},
		{"bitrate", object{
			"play_addr":      address(1080, 1920, 30, 1000, "low"),
			"play_addr_h264": address(1080, 1920, 30, 9000, "high"),
		}, "high"},
		{"rate entries", object{
			"play_addr": address(720, 1280, 60, 9000, "small"),
			"bit_rate": []any{object{
				"fps": 60, "bit_rate": 5000,
				"play_addr": address(1080, 1920, 30, 1000, "full"),
			}},
		}, "full"},
		{"rate metadata", object{
			"play_addr": address(1080, 1920, 30, 9000, "slow"),
			"bit_rate": []any{object{
				"fps": 60, "bit_rate": 5000,
				"play_addr": address(1080, 1920, 30, 1000, "smooth"),
			}, object{
				"fps": 60, "bit_rate": 1000,
				"play_addr": address(1080, 1920, 30, 9000, "low-rate"),
			}},
		}, "smooth"},
		{"stable ties", object{
			"play_addr_h265": address(1080, 1920, 30, 1000, "later"),
			"play_addr":      address(1080, 1920, 30, 1000, "first"),
			"bit_rate":       []any{object{"play_addr": address(1080, 1920, 30, 1000, "rate")}},
		}, "first"},
		{"empty addresses", object{
			"play_addr":      object{"url_list": []any{"", nil}},
			"play_addr_h264": address(720, 1280, 30, 1000, "valid"),
		}, "valid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			for range 10 {
				got := selectVideo(test.video)
				if got == nil || len(got.URLs) != 2 || got.URLs[0] != test.want || got.URLs[1] != test.want+"-backup" {
					t.Fatalf("selected %+v, want %s with its backup", got, test.want)
				}
			}
		})
	}
	if got := selectVideo(object{"bit_rate": []any{nil, "invalid"}}); got != nil {
		t.Fatalf("selected unusable video: %+v", got)
	}
}
