package nitrado

import "testing"

func TestFindDayZServices(t *testing.T) {
	services := []Service{
		{ID: "111", Type: "gameserver", Game: "DayZ", Status: "running"},
		{ID: "222", Type: "gameserver", Game: "Minecraft", Status: "running"},
	}

	matches := FindDayZServices(services)
	if len(matches) != 1 {
		t.Fatalf("expected 1 DayZ service, got %d", len(matches))
	}
	if matches[0].ID != "111" {
		t.Fatalf("expected first match to be DayZ service 111")
	}
}

func TestDecodeServicesParsesUsefulFields(t *testing.T) {
	payload := []byte(`{"data":[{"id":"abc","type":"gameserver","status":"running","game":"DayZ","username":"Player1","location":"Frankfurt","details":{"server_name":"Champions"}}]}`)

	services, err := DecodeServices(payload)
	if err != nil {
		t.Fatalf("DecodeServices returned unexpected error: %v", err)
	}
	if len(services) != 1 {
		t.Fatalf("expected 1 parsed service, got %d", len(services))
	}
	if services[0].Game != "DayZ" {
		t.Fatalf("expected game to be DayZ, got %q", services[0].Game)
	}
	if services[0].Details.ServerName != "Champions" {
		t.Fatalf("expected server name to be Champions, got %q", services[0].Details.ServerName)
	}
}
