package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/security"
)

func (a *App) nitradoEnabled() bool {
	if a == nil || a.Config == nil {
		return false
	}
	return strings.TrimSpace(a.Config.NitradoToken) != ""
}

func nitradoClientFromConnection(cipher security.CredentialCipher, connection repository.NitradoConnection) (*nitrado.Client, error) {
	if cipher == nil {
		return nil, errors.New("credential encryption is not configured")
	}
	token, err := cipher.Decrypt(connection.Ciphertext, connection.Nonce, connection.KeyVersion)
	if err != nil {
		return nil, fmt.Errorf("decrypt Nitrado credential: %w", err)
	}
	if strings.TrimSpace(string(token)) == "" {
		return nil, errors.New("stored Nitrado credential is empty")
	}
	return nitrado.NewClient(nitrado.DefaultBaseURL, string(token), nil), nil
}

func (a *App) nitradoClientForServer(ctx context.Context, row repository.GameServer) (*nitrado.Client, error) {
	if a == nil || a.Servers == nil {
		return nil, errors.New("server repository is not initialized")
	}
	connection, err := a.Servers.GetConnection(ctx, row.GuildID)
	if err != nil {
		return nil, fmt.Errorf("load Nitrado credential for guild %d: %w", row.GuildID, err)
	}
	return nitradoClientFromConnection(a.CredentialCipher, *connection)
}

func (a *App) verifyNitrado(ctx context.Context) (authenticated, verified bool, game, serviceType, status string) {
	if !a.nitradoEnabled() {
		slog.Warn("component=nitrado", "msg", "NITRADO_TOKEN not configured; operating in degraded mode without live service verification")
		return false, false, "", "", ""
	}
	if err := a.Nitrado.AuthenticationCheck(ctx); err != nil {
		logNitradoFailure("authentication", err)
		slog.Warn("component=nitrado", "msg", "Nitrado verification unavailable; continuing in degraded mode so credentials can be repaired with /server connect")
		return false, false, "", "", ""
	}

	services, err := a.Nitrado.GetServices(ctx)
	if err != nil {
		logNitradoFailure("service discovery", err)
		slog.Warn("component=nitrado", "msg", "Nitrado service discovery unavailable; continuing in degraded mode")
		return true, false, "", "", ""
	}
	dayZServices := nitrado.FindDayZServices(services)
	if len(dayZServices) == 0 {
		slog.Warn("component=nitrado", "msg", "no DayZ services discovered")
	} else {
		slog.Info("component=nitrado", "msg", "DayZ services discovered", "count", len(dayZServices))
	}

	if a.Config.NitradoServiceID == "" {
		slog.Warn("component=nitrado", "msg", "NITRADO_SERVICE_ID not configured; skipping service verification and log discovery")
		return true, false, "", "", ""
	}
	service, err := a.Nitrado.ValidateServiceID(ctx, a.Config.NitradoServiceID, services)
	if err != nil {
		slog.Warn("component=nitrado", "operation", "service verification", "msg", "configured service could not be verified; continuing in degraded mode", "service_id", a.Config.NitradoServiceID, "err", err.Error())
		return true, false, "", "", ""
	}
	slog.Info("component=nitrado", "msg", "configured service verified",
		"service_id", a.Config.NitradoServiceID,
		"game", service.Game,
		"service_type", service.Type,
		"status", service.Status,
	)
	if err := a.Nitrado.InspectService(ctx, a.Config.NitradoServiceID); err != nil {
		slog.Warn("component=nitrado", "msg", "service payload inspection failed", "err", err.Error())
	}
	return true, true, service.Game, service.Type, service.Status
}

// logNitradoFailure emits a sanitized, classified failure line. It never
// includes tokens or headers — only HTTP status, operation, and failure kind.
func logNitradoFailure(operation string, err error) {
	var reqErr *nitrado.RequestError
	if errors.As(err, &reqErr) {
		slog.Error("component=nitrado",
			"status", reqErr.StatusCode,
			"operation", operation,
			"kind", string(reqErr.Kind),
			"msg", nitradoFailureMessage(reqErr.Kind),
		)
		return
	}
	slog.Error("component=nitrado", "operation", operation, "kind", string(nitrado.KindTemporary), "msg", err.Error())
}

// nitradoFailureMessage maps a failure kind to a human-readable cause.
func nitradoFailureMessage(kind nitrado.ErrorKind) string {
	switch kind {
	case nitrado.KindAuthentication:
		return "authentication failed"
	case nitrado.KindInvalidEndpoint:
		return "invalid API endpoint"
	case nitrado.KindNotFound:
		return "service ID not found"
	case nitrado.KindPermission:
		return "API permission problem"
	case nitrado.KindTemporary:
		return "temporary Nitrado failure"
	default:
		return "unexpected Nitrado response"
	}
}
