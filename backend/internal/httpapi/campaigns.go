package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"smart-campaign-composer/backend/internal/abtest"
	"smart-campaign-composer/backend/internal/campaign"
	"smart-campaign-composer/backend/internal/content"
	"smart-campaign-composer/backend/internal/language"
	"smart-campaign-composer/backend/internal/segment"
)

type versionView struct {
	ID            int64           `json:"id"`
	Version       int             `json:"version"`
	Source        string          `json:"source"`
	LLMProvider   *string         `json:"llm_provider"`
	Name          string          `json:"name"`
	SegmentRule   json.RawMessage `json:"segment_rule,omitempty"`
	SMS           json.RawMessage `json:"sms,omitempty"`
	Email         json.RawMessage `json:"email,omitempty"`
	Audience      json.RawMessage `json:"audience,omitempty"`
	ChangeSummary string          `json:"change_summary"`
	CreatedAt     time.Time       `json:"created_at"`
	// Localization is the language setup and LanguageSummary how the
	// audience split by language at save time (who waited included).
	Localization    json.RawMessage `json:"localization,omitempty"`
	LanguageSummary json.RawMessage `json:"language_summary,omitempty"`
	// ABTest is the B side of an A/B test.
	ABTest json.RawMessage `json:"ab_test,omitempty"`
}

// getCampaign returns a campaign with its current version in full and its
// version history.
func (s *Server) getCampaign(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathCampaignID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	var (
		chatID, publicID, name, status string
		ordinal                        int
		currentID                      *int64
		approvedID                     *int64
		approval, approvalLanguages    json.RawMessage
		updatedAt                      time.Time
		scheduledAt                    *time.Time
	)
	err := s.d.Pool.QueryRow(ctx, `
		SELECT chat_id::text, public_id::text, ordinal, name, status, current_version_id, approved_version_id, approval,
		       approval_languages, updated_at, scheduled_at
		FROM campaigns WHERE id = $1`, id).Scan(&chatID, &publicID, &ordinal, &name, &status, &currentID, &approvedID, &approval,
		&approvalLanguages, &updatedAt, &scheduledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "campaign_not_found", "kampanya bulunamadı")
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	rows, err := s.d.Pool.Query(ctx, `
		SELECT id, version, source, llm_provider, name, segment_rule, sms, email, audience, change_summary, created_at,
		       localization, language_summary, ab_test
		FROM campaign_versions WHERE campaign_id = $1 ORDER BY version DESC`, id)
	if err != nil {
		internalError(w, err)
		return
	}
	versions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (versionView, error) {
		var v versionView
		err := row.Scan(&v.ID, &v.Version, &v.Source, &v.LLMProvider, &v.Name, &v.SegmentRule, &v.SMS, &v.Email,
			&v.Audience, &v.ChangeSummary, &v.CreatedAt, &v.Localization, &v.LanguageSummary, &v.ABTest)
		return v, err
	})
	if err != nil {
		internalError(w, err)
		return
	}
	var current *versionView
	history := make([]map[string]any, 0, len(versions))
	for i := range versions {
		v := versions[i]
		if currentID != nil && v.ID == *currentID {
			current = &versions[i]
		}
		history = append(history, map[string]any{"id": v.ID, "version": v.Version, "source": v.Source,
			"llm_provider": v.LLMProvider, "change_summary": v.ChangeSummary, "created_at": v.CreatedAt})
	}
	if scheduledAt != nil {
		at := scheduledAt.UTC()
		scheduledAt = &at
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": publicID, "chat_id": chatID, "ordinal": ordinal, "name": name, "status": status,
		"current_version": current, "approved_version_id": approvedID, "approval": approval,
		"approval_languages": approvalLanguages, "versions": history, "updated_at": updatedAt,
		"scheduled_at": scheduledAt,
	})
}

// editRequest is a manual edit. A missing segment_rule, localization or
// ab_test keeps the base version's; ab_test null removes the A/B test.
type editRequest struct {
	BaseVersionID int64              `json:"base_version_id"`
	Name          string             `json:"name"`
	SegmentRule   *segment.Rule      `json:"segment_rule"`
	SMS           *content.SMS       `json:"sms"`
	Email         *content.Email     `json:"email"`
	Localization  *language.Settings `json:"localization"`
	ABTest        optionalABTest     `json:"ab_test"`
}

// optionalABTest tells an absent ab_test from a null one.
type optionalABTest struct {
	Set   bool
	Value *abtest.Test
}

func (o *optionalABTest) UnmarshalJSON(b []byte) error {
	o.Set, o.Value = true, nil
	if string(b) == "null" {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var t abtest.Test
	if err := dec.Decode(&t); err != nil {
		return err
	}
	o.Value = &t
	return nil
}

type createCampaignRequest struct {
	ChatID       string             `json:"chat_id"`
	Name         string             `json:"name"`
	SegmentRule  *segment.Rule      `json:"segment_rule"`
	SMS          *content.SMS       `json:"sms"`
	Email        *content.Email     `json:"email"`
	Localization *language.Settings `json:"localization"`
	ABTest       *abtest.Test       `json:"ab_test"`
}

// createCampaign creates a manual campaign. If no chat is supplied, the
// chat and first version are written atomically. No conversation is forged.
func (s *Server) createCampaign(w http.ResponseWriter, r *http.Request) {
	var req createCampaignRequest
	if !decode(w, r, &req) {
		return
	}
	if req.ChatID != "" && !uuidRe.MatchString(req.ChatID) {
		writeError(w, http.StatusBadRequest, "invalid_chat_id", "sohbet kimliği geçersiz")
		return
	}
	ctx := r.Context()
	tx, err := s.d.Pool.Begin(ctx)
	if err != nil {
		internalError(w, err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	chatID := req.ChatID
	if chatID == "" {
		if err := tx.QueryRow(ctx, `INSERT INTO chats DEFAULT VALUES RETURNING id::text`).Scan(&chatID); err != nil {
			internalError(w, err)
			return
		}
	}
	saved, err := campaign.NewStore(tx).CreateManual(ctx, chatID, campaign.Edit{
		Name: req.Name, Rule: req.SegmentRule, SMS: req.SMS, Email: req.Email, Localization: req.Localization,
		ABTest: req.ABTest, ABTestSet: true,
	}, s.d.Catalog, s.d.Clock.Now())
	if writeCampaignError(w, err) {
		return
	}
	var publicID string
	if err := tx.QueryRow(ctx, `SELECT public_id::text FROM campaigns WHERE id=$1`, saved.CampaignID).Scan(&publicID); err != nil {
		internalError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"campaign_id": publicID, "chat_id": chatID, "ordinal": saved.Ordinal,
		"version": saved.Version, "version_id": saved.VersionID,
		"created": saved.Created, "change_summary": saved.ChangeSummary,
	})
}

// editCampaign saves a manual edit as a new version.
func (s *Server) editCampaign(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathCampaignID(w, r)
	if !ok {
		return
	}
	var req editRequest
	if !decode(w, r, &req) {
		return
	}
	saved, err := s.d.Campaigns.EditVersionWithRule(r.Context(), id, req.BaseVersionID,
		campaign.Edit{Name: req.Name, Rule: req.SegmentRule, SMS: req.SMS, Email: req.Email, Localization: req.Localization,
			ABTest: req.ABTest.Value, ABTestSet: req.ABTest.Set},
		s.d.Catalog, s.d.Clock.Now())
	if writeCampaignError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"campaign_id": chi.URLParam(r, "campaignID"), "ordinal": saved.Ordinal, "version": saved.Version,
		"version_id": saved.VersionID, "created": saved.Created, "change_summary": saved.ChangeSummary,
	})
}

// approveRequest approves a version; scheduled_at (RFC 3339, any offset)
// schedules the sending, and its absence sends now.
type approveRequest struct {
	VersionID   int64      `json:"version_id"`
	ScheduledAt *time.Time `json:"scheduled_at"`
}

// approveCampaign approves the given (current) version.
func (s *Server) approveCampaign(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathCampaignID(w, r)
	if !ok {
		return
	}
	var req approveRequest
	if !decode(w, r, &req) {
		return
	}
	res, err := s.d.Campaigns.ApproveWith(r.Context(), id, req.VersionID, s.d.Catalog, s.d.Clock.Now(),
		campaign.ApproveOptions{SendAt: req.ScheduledAt})
	if writeCampaignError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"campaign_id": chi.URLParam(r, "campaignID"), "version_id": res.VersionID,
		"status": res.Status, "already_approved": res.AlreadyApproved, "channels": res.Channels,
		"languages": res.Languages, "excluded_language": res.ExcludedLanguage, "scheduled_at": res.ScheduledAt,
		"variants": res.Variants,
	})
}

// cancelCampaign stops an approved campaign whose scheduled time has not
// arrived. A started or immediately approved send cannot be cancelled.
func (s *Server) cancelCampaign(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathCampaignID(w, r)
	if !ok {
		return
	}
	if err := s.d.Campaigns.CancelScheduled(r.Context(), id); err != nil {
		if errors.Is(err, campaign.ErrNotCancellable) {
			writeError(w, http.StatusConflict, "cancellation_closed", "gönderim başlamış veya zamanı gelmiş; kampanya iptal edilemez")
			return
		}
		writeCampaignError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "cancelled"})
}

// previewApproval shows what approving the version would do now, writing
// nothing: counts per language and channel, who waits for a language
// decision or is left out, and one rendered message per language and
// channel.
func (s *Server) previewApproval(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathCampaignID(w, r)
	if !ok {
		return
	}
	versionID, err := strconv.ParseInt(r.URL.Query().Get("version_id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_version_id", "versiyon kimliği geçersiz")
		return
	}
	p, err := s.d.Campaigns.PreviewApproval(r.Context(), id, versionID, s.d.Catalog, s.d.Clock.Now())
	if writeCampaignError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// writeCampaignError maps campaign errors to responses; it reports whether
// it wrote one.
func writeCampaignError(w http.ResponseWriter, err error) bool {
	var (
		stale    *campaign.StaleError
		ve       *campaign.ValidationError
		decision *campaign.DecisionError
	)
	switch {
	case err == nil:
		return false
	case errors.Is(err, campaign.ErrNotFound):
		writeError(w, http.StatusNotFound, "campaign_not_found", "kampanya bulunamadı")
	case errors.Is(err, campaign.ErrNotDraft):
		writeError(w, http.StatusConflict, "not_draft", "kampanya onaylanmış; değiştirilemez")
	case errors.As(err, &stale):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "stale_version", "current_version": stale.CurrentVersion,
			"message": "kampanya bu arada değişti; güncel versiyonu yükleyip tekrar deneyin"})
	case errors.As(err, &decision):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "language_decision_required", "groups": decision.Groups,
			"message": "gönderim başlamadı: bazı alıcılar için dil kararı gerekiyor"})
	case errors.As(err, &ve):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "validation_failed", "issues": ve.Issues,
			"message": "kampanya bu haliyle kaydedilemez ya da onaylanamaz"})
	default:
		internalError(w, err)
	}
	return true
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "istek gövdesi okunamadı")
		return false
	}
	return true
}
