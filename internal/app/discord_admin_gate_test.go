package app

import (
	"testing"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// One Discord server, two installations (two DayZ servers): a role mapped ADMINISTRATOR on one is
// an admin for that server only, and for the guild as a whole only when mapped on both.
func TestInstallationAdminIsPerInstallation(t *testing.T) {
	rows := []repository.InstallationRoleLevel{
		{InstallationID: 1, GameServerID: 10, Level: "ADMINISTRATOR"},
		{InstallationID: 1, GameServerID: 10, Level: "MODERATOR"},
		{InstallationID: 2, GameServerID: 20, Level: ""},
	}
	if !installationAdmin(rows, 10) {
		t.Fatal("mapped ADMINISTRATOR on server 10's installation")
	}
	if installationAdmin(rows, 20) {
		t.Fatal("server 20's installation has no mapping")
	}
	if installationAdmin(rows, 30) {
		t.Fatal("an unknown server grants nothing")
	}
	if installationAdmin(rows, 0) {
		t.Fatal("a guild-wide check needs the level on every installation")
	}
	rows[2].Level = "OWNER"
	if !installationAdmin(rows, 0) || !installationAdmin(rows, 20) {
		t.Fatal("OWNER on the second installation completes the guild-wide grant")
	}
	if installationAdmin(nil, 0) || installationAdmin(nil, 10) {
		t.Fatal("no installations, no admin")
	}
	only := []repository.InstallationRoleLevel{{InstallationID: 3, GameServerID: 40, Level: "MODERATOR"}}
	if installationAdmin(only, 0) || installationAdmin(only, 40) {
		t.Fatal("MODERATOR is not an admin")
	}
}
