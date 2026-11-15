package agent

import "testing"

// smartctl's output is parsed on every health check; odd disks and odd
// smartctl versions must not crash the agent.
func FuzzParseSmart(f *testing.F) {
	for _, s := range []string{
		`{"smartctl":{"exit_status":0},"smart_status":{"passed":true},"nvme_smart_health_information_log":{"available_spare":100,"available_spare_threshold":10,"percentage_used":85}}`,
		`{"smartctl":{"exit_status":2}}`,
		`{"smartctl":{"exit_status":0},"smart_status":{"passed":false},"ata_smart_attributes":{"table":[{"id":5,"name":"Reallocated_Sector_Ct","raw":{"value":12}}]},"temperature":{"current":61}}`,
		`{}`, `[]`, `null`, `{"ata_smart_attributes":{"table":[null]}}`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, out string) {
		_, _ = parseSmart("/dev/sda", []byte(out))
	})
}

// A discovery command's output (a gateway's Bluetooth scan) is parsed on
// every scan.
func FuzzParseReports(f *testing.F) {
	for _, s := range []string{
		`[{"address":"aa:bb:cc:dd:ee:ff","name":"sensor","model":"s1","firmware_version":"1.0","rssi":-60}]`,
		`{"address":"aa:bb:cc:dd:ee:ff"}` + "\n" + `{"address":"11:22:33:44:55:66","battery_percent":50}`,
		``, `[]`, `[null]`, `{"address":""}`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, out string) {
		_, _ = parseReports(out, DiscoveryConfig{Type: "command", Model: "s1", Protocol: "mcumgr"})
	})
}
