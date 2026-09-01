package temporal

import (
	"bytes"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const pinnedTZDBSubprocess = "SHEETS_PINNED_TZDB_SUBPROCESS"

func TestPinnedZoneDatabaseIgnoresPoisonedHostSources(t *testing.T) {
	if os.Getenv(pinnedTZDBSubprocess) == "1" {
		assertPinnedZoneGoldens(t)
		return
	}
	poisonRoot := t.TempDir()
	utcData := pinnedZoneDataForTest(t, "UTC")
	for _, name := range []string{"Europe/Stockholm", "Australia/Lord_Howe", "Pacific/Apia"} {
		path := filepath.Join(poisonRoot, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, utcData, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command(os.Args[0], "-test.run=^TestPinnedZoneDatabaseIgnoresPoisonedHostSources$", "-test.count=1")
	command.Env = append(environmentWithout(os.Environ(), "ZONEINFO", pinnedTZDBSubprocess),
		"ZONEINFO="+poisonRoot, pinnedTZDBSubprocess+"=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("poisoned timezone subprocess: %v\n%s", err, output)
	}
}

func assertPinnedZoneGoldens(t *testing.T) {
	// Establish that the subprocess's ordinary Go loader really did consume
	// the hostile ZONEINFO file before exercising Sheets's independent loader.
	host, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	_, hostOffset := time.Date(1818, 7, 21, 21, 40, 32, 0, host).Zone()
	if hostOffset != 0 {
		t.Fatalf("poisoned host offset = %d, want UTC", hostOffset)
	}

	for input, expected := range map[string]string{
		"1818-07-21T21:40:32.142[Europe/Stockholm]":   "1818-07-21T21:40:32.142+00:53:28[Europe/Stockholm]",
		"2017-10-29T02:30[Europe/Stockholm]":          "2017-10-29T02:30+02:00[Europe/Stockholm]",
		"2017-10-29T02:30+01:00[Europe/Stockholm]":    "2017-10-29T02:30+01:00[Europe/Stockholm]",
		"2017-03-26T02:30[Europe/Stockholm]":          "2017-03-26T03:30+02:00[Europe/Stockholm]",
		"2017-10-01T02:15[Australia/Lord_Howe]":       "2017-10-01T02:45+11:00[Australia/Lord_Howe]",
		"2018-04-01T01:45[Australia/Lord_Howe]":       "2018-04-01T01:45+11:00[Australia/Lord_Howe]",
		"2018-04-01T01:45+10:30[Australia/Lord_Howe]": "2018-04-01T01:45+10:30[Australia/Lord_Howe]",
		"2011-12-30T12:00[Pacific/Apia]":              "2011-12-31T12:00+14:00[Pacific/Apia]",
	} {
		value, parseErr := ParseDateTime(input)
		if parseErr != nil || value.String() != expected {
			t.Errorf("ParseDateTime(%q) = %q, %v; want %q", input, value, parseErr, expected)
		}
	}
	historical, err := ParseDateTime("1818-07-21T21:40:32.142[Europe/Stockholm]")
	if err != nil {
		t.Fatal(err)
	}
	binary, err := historical.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	const historicalBinary = "01fffffffee322c6480876bf800100000c8800104575726f70652f53746f636b686f6c6d"
	if encoded := hex.EncodeToString(binary); encoded != historicalBinary {
		t.Fatalf("historical Stockholm binary = %s, want %s", encoded, historicalBinary)
	}

	before, err := ParseDateTime("2017-10-28T23:00+02:00[Europe/Stockholm]")
	if err != nil {
		t.Fatal(err)
	}
	oneDay, _ := NewDuration(0, 1, 0, 0)
	after, err := before.Add(oneDay)
	if err != nil || after.String() != "2017-10-29T23:00+01:00[Europe/Stockholm]" {
		t.Fatalf("pinned arithmetic = %s, %v", after, err)
	}
	truncated, err := after.Truncate("week")
	if err != nil || truncated.String() != "2017-10-23T00:00+02:00[Europe/Stockholm]" {
		t.Fatalf("pinned truncation = %s, %v", truncated, err)
	}
}

func TestPinnedZoneDatabaseProvenanceCompatibilityAndErrors(t *testing.T) {
	if PinnedTZDBVersion != "2023c" ||
		PinnedTZDBProfile != "main (PACKRATDATA=, PACKRATLIST=)" ||
		PinnedTZDBSHA256 != "3fe2fe0c5897093e4965480de18722eabc224a1b7ac4dcb1ceb6943d62c01efe" {
		t.Fatalf("unexpected pinned timezone provenance %q %q %q",
			PinnedTZDBVersion, PinnedTZDBProfile, PinnedTZDBSHA256)
	}
	pinned, err := (PinnedZoneDatabase{}).LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	compatibility, err := (GoZoneDatabase{}).LoadLocation("Europe/Stockholm")
	if err != nil || pinned != compatibility {
		t.Fatalf("compatibility provider = %p, %v; pinned = %p", compatibility, err, pinned)
	}
	if _, err := (PinnedZoneDatabase{}).LoadLocation("Local"); err == nil || !strings.Contains(err.Error(), PinnedTZDBVersion) {
		t.Fatalf("host-local timezone error = %v", err)
	}
	if _, err := (PinnedZoneDatabase{}).LoadLocation("Not/A_Real_Zone"); err == nil || !strings.Contains(err.Error(), "unknown timezone") {
		t.Fatalf("unknown timezone error = %v", err)
	}
}

func TestPinnedZoneDatabaseUsesM23MainProfileAcrossBackzoneAliases(t *testing.T) {
	// IANA 2022b moved these zones to backzone because their timestamps have
	// agreed with the target since 1970. M23 follows IANA 2023c's default main
	// profile, so every alias must retain the target's complete TZif payload.
	// Testing the full move set prevents a Stockholm-specific compatibility
	// patch from masquerading as the selected database policy.
	aliases := map[string]string{
		"Antarctica/Vostok":  "Asia/Urumqi",
		"Asia/Brunei":        "Asia/Kuching",
		"Asia/Kuala_Lumpur":  "Asia/Singapore",
		"Atlantic/Reykjavik": "Africa/Abidjan",
		"Europe/Amsterdam":   "Europe/Brussels",
		"Europe/Copenhagen":  "Europe/Berlin",
		"Europe/Luxembourg":  "Europe/Brussels",
		"Europe/Monaco":      "Europe/Paris",
		"Europe/Oslo":        "Europe/Berlin",
		"Europe/Stockholm":   "Europe/Berlin",
		"Indian/Christmas":   "Asia/Bangkok",
		"Indian/Cocos":       "Asia/Yangon",
		"Indian/Kerguelen":   "Indian/Maldives",
		"Indian/Mahe":        "Asia/Dubai",
		"Indian/Reunion":     "Asia/Dubai",
		"Pacific/Chuuk":      "Pacific/Port_Moresby",
		"Pacific/Funafuti":   "Pacific/Tarawa",
		"Pacific/Majuro":     "Pacific/Tarawa",
		"Pacific/Pohnpei":    "Pacific/Guadalcanal",
		"Pacific/Wake":       "Pacific/Tarawa",
		"Pacific/Wallis":     "Pacific/Tarawa",
	}
	for alias, target := range aliases {
		aliasData := pinnedZoneDataForTest(t, alias)
		targetData := pinnedZoneDataForTest(t, target)
		if !bytes.Equal(aliasData, targetData) {
			t.Errorf("%s payload differs from main-profile target %s", alias, target)
		}
	}
}

func TestPinnedZoneDatabaseLoadsCompleteProfile(t *testing.T) {
	pinnedTZDBOnce.Do(initializePinnedTZDB)
	if pinnedTZDBInitError != nil {
		t.Fatal(pinnedTZDBInitError)
	}
	if len(pinnedTZDBFiles) != 597 {
		t.Fatalf("pinned timezone count = %d, want 597", len(pinnedTZDBFiles))
	}
	database := PinnedZoneDatabase{}
	for name := range pinnedTZDBFiles {
		location, err := database.LoadLocation(name)
		if err != nil {
			t.Errorf("load pinned timezone %q: %v", name, err)
			continue
		}
		if location.String() != name {
			t.Errorf("pinned timezone %q loaded as %q", name, location)
		}
	}
}

func TestPreviouslyResolvedOffsetsRemainBinaryStable(t *testing.T) {
	// Persisted DateTimes are resolved snapshots: changing the rules used for
	// new construction must neither reject nor reinterpret values created by
	// the old macOS host loader or Sheets's previous Go backzone archive.
	for name, test := range map[string]struct {
		binary string
		value  string
	}{
		"macOS main profile": {
			binary: "01fffffffee322c6480876bf800100000c8800104575726f70652f53746f636b686f6c6d",
			value:  "1818-07-21T21:40:32.142+00:53:28[Europe/Stockholm]",
		},
		"previous Go backzone profile": {
			binary: "01fffffffee322c1e40876bf8001000010ec00104575726f70652f53746f636b686f6c6d",
			value:  "1818-07-21T21:40:32.142+01:12:12[Europe/Stockholm]",
		},
	} {
		t.Run(name, func(t *testing.T) {
			payload, err := hex.DecodeString(test.binary)
			if err != nil {
				t.Fatal(err)
			}
			value, err := DateTimeFromBinary(payload)
			if err != nil || value.String() != test.value {
				t.Fatalf("legacy resolved value = %s, %v; want %s", value, err, test.value)
			}
			reencoded, err := value.MarshalBinary()
			if err != nil || !bytes.Equal(reencoded, payload) {
				t.Fatalf("legacy resolved binary changed to %x, %v", reencoded, err)
			}
		})
	}
}

type recordingZoneDatabase struct {
	location *time.Location
	calls    int
}

func (d *recordingZoneDatabase) LoadLocation(string) (*time.Location, error) {
	d.calls++
	return d.location, nil
}

func TestExplicitZoneDatabaseInjectionRemainsAuthoritative(t *testing.T) {
	local, err := ParseLocalDateTime("2020-01-02T03:04:05")
	if err != nil {
		t.Fatal(err)
	}
	database := &recordingZoneDatabase{location: time.FixedZone("injected", 20*60+34)}
	value, err := NewDateTimeWithDatabase(local, "Europe/Stockholm", database)
	if err != nil || value.String() != "2020-01-02T03:04:05+00:20:34[Europe/Stockholm]" || database.calls != 1 {
		t.Fatalf("injected construction = %s, calls=%d, %v", value, database.calls, err)
	}
	oneDay, _ := NewDuration(0, 1, 0, 0)
	value, err = value.AddWithDatabase(oneDay, database)
	if err != nil || value.String() != "2020-01-03T03:04:05+00:20:34[Europe/Stockholm]" || database.calls != 3 {
		t.Fatalf("injected arithmetic = %s, calls=%d, %v", value, database.calls, err)
	}
	value, err = value.TruncateWithDatabase("day", database)
	if err != nil || value.String() != "2020-01-03T00:00+00:20:34[Europe/Stockholm]" || database.calls != 4 {
		t.Fatalf("injected truncation = %s, calls=%d, %v", value, database.calls, err)
	}
}

func pinnedZoneDataForTest(t *testing.T, name string) []byte {
	t.Helper()
	pinnedTZDBOnce.Do(initializePinnedTZDB)
	if pinnedTZDBInitError != nil {
		t.Fatal(pinnedTZDBInitError)
	}
	file := pinnedTZDBFiles[name]
	if file == nil {
		t.Fatalf("pinned timezone %q is absent", name)
	}
	reader, err := file.Open()
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if closeErr := reader.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func environmentWithout(environment []string, names ...string) []string {
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		keep := true
		for _, name := range names {
			if strings.HasPrefix(entry, name+"=") {
				keep = false
				break
			}
		}
		if keep {
			result = append(result, entry)
		}
	}
	return result
}
