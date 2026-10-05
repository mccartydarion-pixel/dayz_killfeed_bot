package discord

import "strings"

// One voice for everything the bot says back to a person: the private answers to commands,
// buttons and forms. docs/DISCORD_DESIGN.md "Voice" is the written form of these rules.
//
//   - Calm, plain, sentence case. Say what happened, then what to do next.
//   - A refusal or a failure carries no emoji and never blames the person.
//   - No internal names: no error classes, no ids, no "database", no stack-like text.
//     A detail that helps the person (which permission, which channel, which command) stays.
//   - A success may start with one ✅.
//
// The sentences more than one command needs live here, so the wording cannot drift from one
// command to the next. A message only one command needs stays beside that command, written
// the same way.

const (
	// ReplyTryAgain answers when something failed on Champion's side and trying again may work.
	ReplyTryAgain = "Something went wrong on our side. Try again in a moment."
	// ReplyCommandUnavailable answers a command nothing is registered for (a feature that is off).
	ReplyCommandUnavailable = "This command is unavailable right now."
	// ReplyButtonExpired answers a button or form left on an old message.
	ReplyButtonExpired = "This button is no longer valid."

	// ReplyNotSetUp answers a command used before the server was set up.
	ReplyNotSetUp = "This server isn't set up yet. Run `/setup run` first."
	// ReplyNoServerSelected answers a player command when the Discord has no DayZ server picked.
	ReplyNoServerSelected = "No DayZ server is selected for this Discord yet."
	// ReplyNeedsManageServer refuses a staff action without naming it.
	ReplyNeedsManageServer = "You need the Administrator or Manage Server permission to do that."
	// ReplyNotYourConfirmation refuses a confirmation button pressed by someone else.
	ReplyNotYourConfirmation = "This confirmation belongs to another member."
	// ReplyPlayerNotFound answers a lookup that matched no player.
	ReplyPlayerNotFound = "No player with that name was found."
	// ReplyChooseSubcommand answers a command sent without one of its subcommands.
	ReplyChooseSubcommand = "Choose a subcommand."

	// replyNotLinkedHeader starts every "link your account first" answer.
	replyNotLinkedHeader = "🔗 **Account not linked**\n"
)

// ReplyNeedsPermission refuses a staff action and says which one:
// "You need the Administrator or Manage Server permission to manage servers."
func ReplyNeedsPermission(action string) string {
	return "You need the Administrator or Manage Server permission to " + strings.TrimSpace(action) + "."
}

// ReplyIsUnavailable says a feature cannot be used right now: "The economy is unavailable
// right now. Try again later." ReplyAreUnavailable is the same for a plural ("Stats are ...").
func ReplyIsUnavailable(feature string) string {
	return strings.TrimSpace(feature) + " is unavailable right now. Try again later."
}

// ReplyAreUnavailable is ReplyIsUnavailable for a plural feature name.
func ReplyAreUnavailable(feature string) string {
	return strings.TrimSpace(feature) + " are unavailable right now. Try again later."
}

// ReplyCouldNot says an action failed on Champion's side and may work on a second try:
// "Couldn't load your stats right now. Try again in a moment."
func ReplyCouldNot(action string) string {
	return "Couldn't " + strings.TrimSpace(action) + " right now. Try again in a moment."
}

// ReplyCouldNotBecause says an action was not done and why, when the reason is one the person
// can act on (a rule they hit, a value that is not allowed): "Couldn't start the season: a
// season is already active." The reason is given as written, with one full stop at the end.
func ReplyCouldNotBecause(action, reason string) string {
	reason = strings.TrimRight(strings.TrimSpace(reason), ".")
	if reason == "" {
		return ReplyCouldNot(action)
	}
	return "Couldn't " + strings.TrimSpace(action) + ": " + reason + "."
}

// ReplyNotLinked tells a player their Discord account is not linked to a player yet, and the
// next step for the command they used ("Link your gamertag with `/link` first.").
func ReplyNotLinked(nextStep string) string {
	return replyNotLinkedHeader + strings.TrimSpace(nextStep)
}

// The texts the interaction router answers with are the reference tone for all of the above.
const (
	interactionFailureText = ReplyTryAgain
	unroutedCommandText    = ReplyCommandUnavailable
	unroutedComponentText  = ReplyButtonExpired
)
