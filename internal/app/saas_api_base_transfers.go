package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Base transfers (docs/BASE_TRANSFERS.md): a base owner asks to hand one of
// their bases to an active, verified member of their faction; the server
// owner approves (the base changes owner) or declines. Both players get a DM.

type baseTransferCreateBody struct {
	BaseID     int64 `json:"baseId"`
	ToPlayerID int64 `json:"toPlayerId"`
}

type baseTransferDeclineBody struct {
	Reason string `json:"reason"`
}

// handleGetPlayerBaseTransfers is GET .../security-marketplace/base-transfers:
// the player's bases, faction mates they could hand one to, and their transfers.
func (a *App) handleGetPlayerBaseTransfers(w http.ResponseWriter, r *http.Request) {
	s, playerID, ctx, cancel, ok := a.playerBaseRequestContext(w, r)
	if !ok {
		return
	}
	defer cancel()
	repo := repository.NewBaseTransferRepository(a.DB.Pool)
	bases, mates, err := repo.Options(ctx, s, playerID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load base transfers")
		return
	}
	transfers, err := repo.Mine(ctx, s, playerID, 10)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load base transfers")
		return
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"bases": bases, "mates": mates, "transfers": transfers, "playerId": playerID})
}

// handleCreatePlayerBaseTransfer is POST .../security-marketplace/base-transfers.
func (a *App) handleCreatePlayerBaseTransfer(w http.ResponseWriter, r *http.Request) {
	var body baseTransferCreateBody
	if !decodeFactionBody(w, r, &body) {
		return
	}
	if body.BaseID <= 0 || body.ToPlayerID <= 0 {
		writeSaaSError(w, codeInvalidRequest, "choose a base and a faction mate")
		return
	}
	s, playerID, ctx, cancel, ok := a.playerBaseRequestContext(w, r)
	if !ok {
		return
	}
	defer cancel()
	t, err := repository.NewBaseTransferRepository(a.DB.Pool).Create(ctx, s, playerID, body.BaseID, body.ToPlayerID)
	switch {
	case errors.Is(err, repository.ErrInvalidBaseTransfer):
		writeSaaSError(w, codeInvalidRequest, "choose a base and a faction mate")
		return
	case errors.Is(err, repository.ErrBaseTransferNotMate):
		writeSaaSError(w, codeConflict, "you can only hand your own base to an active faction mate with a linked Discord")
		return
	case errors.Is(err, repository.ErrBaseTransferLimit):
		writeSaaSError(w, codeConflict, "your faction mate already has the most bases allowed")
		return
	case errors.Is(err, repository.ErrBaseTransferWaiting):
		writeSaaSError(w, codeConflict, "this base already has a transfer waiting for the server owner")
		return
	case err != nil:
		slog.Warn("component=base_transfers", "event", "create_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "the transfer couldn't be requested right now")
		return
	}
	a.notifyNewBaseTransfer(s, t)
	writeSaaSJSON(w, http.StatusCreated, map[string]any{"transfer": t})
}

// handleCancelPlayerBaseTransfer is POST .../security-marketplace/base-transfers/{transferID}/cancel.
func (a *App) handleCancelPlayerBaseTransfer(w http.ResponseWriter, r *http.Request) {
	transferID, good := pathInt64(w, r, "transferID")
	if !good {
		return
	}
	s, playerID, ctx, cancel, ok := a.playerBaseRequestContext(w, r)
	if !ok {
		return
	}
	defer cancel()
	err := repository.NewBaseTransferRepository(a.DB.Pool).Cancel(ctx, s, playerID, transferID)
	switch {
	case errors.Is(err, repository.ErrBaseTransferNotFound):
		writeSaaSError(w, codeNotFound, "no waiting transfer of yours with that id")
		return
	case err != nil:
		writeSaaSError(w, codeInternalError, "the transfer couldn't be cancelled right now")
		return
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"cancelled": true})
}

// handleGetBaseTransfers is GET .../admin/case/base-transfers.
func (a *App) handleGetBaseTransfers(w http.ResponseWriter, r *http.Request) {
	_, s, ok := a.ownerBaseRequestScope(w, r, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	transfers, err := repository.NewBaseTransferRepository(a.DB.Pool).ForOwner(ctx, s, 50)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load base transfers")
		return
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"transfers": transfers})
}

// handleApproveBaseTransfer is POST .../admin/case/base-transfers/{transferID}/approve.
func (a *App) handleApproveBaseTransfer(w http.ResponseWriter, r *http.Request) {
	a.decideBaseTransfer(w, r, true)
}

// handleDeclineBaseTransfer is POST .../admin/case/base-transfers/{transferID}/decline {reason}.
func (a *App) handleDeclineBaseTransfer(w http.ResponseWriter, r *http.Request) {
	a.decideBaseTransfer(w, r, false)
}

func (a *App) decideBaseTransfer(w http.ResponseWriter, r *http.Request, approve bool) {
	transferID, good := pathInt64(w, r, "transferID")
	if !good {
		return
	}
	ac, s, ok := a.ownerBaseRequestScope(w, r, true)
	if !ok {
		return
	}
	var body baseTransferDeclineBody
	if !approve {
		if err := readCaseBaseJSON(w, r, &body); err != nil {
			writeSaaSError(w, codeInvalidRequest, "invalid decline")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	var actor *int64
	if ac.user != nil {
		actor = &ac.user.ID
	}
	repo := repository.NewBaseTransferRepository(a.DB.Pool)
	var d repository.BaseTransferDecision
	var err error
	if approve {
		d, err = repo.Approve(ctx, s, transferID, actor)
	} else {
		d, err = repo.Decline(ctx, s, transferID, body.Reason, actor)
	}
	switch {
	case errors.Is(err, repository.ErrBaseTransferNotFound):
		writeSaaSError(w, codeNotFound, "base transfer not found")
		return
	case errors.Is(err, repository.ErrBaseTransferDecided):
		writeSaaSError(w, codeConflict, "this transfer was already answered or cancelled")
		return
	case errors.Is(err, repository.ErrBaseTransferNotMate):
		writeSaaSError(w, codeConflict, "they're no longer faction mates, or the base changed; decline it instead")
		return
	case errors.Is(err, repository.ErrBaseTransferLimit):
		writeSaaSError(w, codeConflict, "the new owner already has the most bases allowed")
		return
	case errors.Is(err, repository.ErrInvalidBaseTransfer):
		writeSaaSError(w, codeInvalidRequest, "the reason can be up to 300 characters")
		return
	case err != nil:
		slog.Warn("component=base_transfers", "event", "decision_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "the transfer couldn't be answered right now")
		return
	}
	action := "BASE_TRANSFER_DECLINED"
	if approve {
		action = "BASE_TRANSFER_APPROVED"
	}
	a.recordAudit(ctx, ac, action, "base-transfer", "", "success", nil, map[string]any{
		"transferId": transferID, "baseId": d.Transfer.BaseID, "fromPlayerId": d.Transfer.FromPlayerID, "toPlayerId": d.Transfer.ToPlayerID})
	a.notifyBaseTransferDecision(s.ServerID, d)
	writeSaaSJSON(w, http.StatusOK, map[string]any{"transfer": d.Transfer})
}

// notifyNewBaseTransfer posts a staff notice and DMs the organization owner, in the background.
func (a *App) notifyNewBaseTransfer(s repository.BaseRequestScope, t repository.BaseTransfer) {
	if a.DB == nil || a.DB.Pool == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if a.AdminAlerts != nil {
			a.AdminAlerts.Publish(discord.AdminAlert{GuildRowID: s.GuildID, ServerID: s.ServerID, Kind: discord.AlertKindBaseRequest,
				Severity: discord.AlertInfo, Headline: "BASE TRANSFER REQUEST",
				Detail: presentation.SafeName(t.FromName, 64) + " wants to hand **" + presentation.SafeName(t.BaseName, 64) + "** to " +
					presentation.SafeName(t.ToName, 64) + ". Approve or decline it on the anti-cheat Bases tab."})
		}
		ownerID, _, err := repository.NewCaseBaseRequestRepository(a.DB.Pool).RequestNotice(ctx, s, t.FromPlayerID)
		if err != nil || ownerID == "" || a.Discord == nil || a.Discord.Session() == nil {
			return
		}
		session := a.Discord.Session()
		msg := discord.NewBaseTransferMessage(t.FromName, t.ToName, t.BaseName, a.serverName(s.ServerID), a.baseRequestsReviewURL())
		ch, err := session.UserChannelCreate(ownerID)
		if err == nil {
			_, err = session.ChannelMessageSendComplex(ch.ID, msg)
		}
		if err != nil {
			slog.Warn("component=base_transfers", "event", "owner_dm_failed", "transfer_id", t.ID, "err", err.Error())
		}
	}()
}

// notifyBaseTransferDecision DMs both players in the background.
func (a *App) notifyBaseTransferDecision(serverID int64, d repository.BaseTransferDecision) {
	if a.Discord == nil || a.Discord.Session() == nil {
		return
	}
	session := a.Discord.Session()
	server := a.serverName(serverID)
	approved := d.Transfer.Status == repository.BaseTransferApproved
	send := func(userID string, receiving bool) {
		if userID == "" {
			return
		}
		msg := discord.BaseTransferDecisionMessage(approved, receiving, d.Transfer.BaseName, d.Transfer.FromName, d.Transfer.ToName, server, d.Transfer.DeclineReason)
		go func() {
			ch, err := session.UserChannelCreate(userID)
			if err == nil {
				_, err = session.ChannelMessageSendComplex(ch.ID, msg)
			}
			if err != nil {
				slog.Warn("component=base_transfers", "event", "dm_failed", "transfer_id", d.Transfer.ID, "err", err.Error())
			}
		}()
	}
	send(d.FromDiscord, false)
	send(d.ToDiscord, true)
}
