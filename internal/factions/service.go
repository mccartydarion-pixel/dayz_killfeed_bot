package factions
package factions

import (
	"errors"
	"strings"
)

const (
	RoleOwner  = "OWNER"
	RoleLeader = "LEADER"
	RoleOfficer = "OFFICER"
	RoleMember = "MEMBER"
)

type Capability string
const (
	CanInvite Capability = "INVITE"
	CanKick Capability = "KICK"
	CanPromote Capability = "PROMOTE"
	CanDemote Capability = "DEMOTE"
	CanTransfer Capability = "TRANSFER"
	CanDisband Capability = "DISBAND"
)

var ErrInvalidFaction = errors.New("invalid faction name or tag")
var ErrAlreadyMember = errors.New("player already belongs to a faction")
var ErrPermissionDenied = errors.New("faction permission denied")
var ErrOwnerCannotLeave = errors.New("owner must transfer ownership or disband first")

func ValidateNameTag(name, tag string) error {
	name = strings.TrimSpace(name)
	tag = strings.TrimSpace(tag)
	if len([]rune(name)) < 2 || len([]rune(name)) > 24 || len([]rune(tag)) < 2 || len([]rune(tag)) > 5 {
		return ErrInvalidFaction
	}
	for _, r := range name + tag {
		if r < 0x20 || r == '@' { return ErrInvalidFaction }
	}
	return nil
}

func Can(role string, capability Capability) bool {
	switch capability {
	case CanInvite:
		return role == RoleOwner || role == RoleLeader || role == RoleOfficer
	case CanKick:
		return role == RoleOwner || role == RoleLeader || role == RoleOfficer
	case CanPromote, CanDemote:
		return role == RoleOwner || role == RoleLeader
	case CanTransfer, CanDisband:
		return role == RoleOwner
	default:
		return false
	}
}

func IsValidRole(role string) bool {
	return role == RoleOwner || role == RoleLeader || role == RoleOfficer || role == RoleMember
}
