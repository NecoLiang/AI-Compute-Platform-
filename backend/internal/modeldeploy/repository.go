package modeldeploy

import (
	"database/sql"
	"errors"

	"github.com/jmoiron/sqlx"
)

type Repository struct{ db *sqlx.DB }

func NewRepository(db *sqlx.DB) *Repository { return &Repository{db: db} }

func (r *Repository) ListEntries(status string) ([]ModelEntry, error) {
	q, args := "SELECT "+entryColumns+" FROM model_catalog", []any{}
	if status != "" {
		q += " WHERE status=?"
		args = append(args, status)
	}
	list := []ModelEntry{}
	err := r.db.Select(&list, q+" ORDER BY id DESC", args...)
	return list, err
}

// GetEntry 不存在返回 (nil, nil)。
func (r *Repository) GetEntry(id int64) (*ModelEntry, error) {
	var e ModelEntry
	err := r.db.Get(&e, "SELECT "+entryColumns+" FROM model_catalog WHERE id=?", id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func lockEntryTx(tx *sqlx.Tx, id int64) (*ModelEntry, error) {
	var e ModelEntry
	err := tx.Get(&e, "SELECT "+entryColumns+" FROM model_catalog WHERE id=? FOR UPDATE", id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (r *Repository) InsertEntry(e *ModelEntry) error {
	res, err := r.db.Exec(`INSERT INTO model_catalog
		(model_key, display_name, source, repo, revision, manifest, weights_bytes, quant,
		 min_compute_capability, valid_tp, kv_bytes_per_token, kv_replicated_across_tp, runtime_overhead_bytes,
		 max_model_len, min_context_tokens, engine, engine_image, min_driver_version, engine_args, metrics_map,
		 startup_timeout_s, license, commercial_use, status, created_by)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'draft',?)`,
		e.ModelKey, e.DisplayName, e.Source, e.Repo, e.Revision, []byte(e.Manifest), e.WeightsBytes, e.Quant,
		e.MinComputeCapability, e.ValidTP, e.KVBytesPerToken, e.KVReplicatedAcrossTP, e.RuntimeOverheadBytes,
		e.MaxModelLen, e.MinContextTokens, e.Engine, e.EngineImage, e.MinDriverVersion,
		[]byte(e.EngineArgs), []byte(e.MetricsMap), e.StartupTimeoutS, e.License, e.CommercialUse, e.CreatedBy)
	if err != nil {
		return err
	}
	e.ID, _ = res.LastInsertId()
	return nil
}

// updateDraftTx 守卫式整体更新: 只有 draft 行会被改写。
func updateDraftTx(tx *sqlx.Tx, id int64, e *ModelEntry) (int64, error) {
	res, err := tx.Exec(`UPDATE model_catalog SET
		model_key=?, display_name=?, source=?, repo=?, revision=?, manifest=?, weights_bytes=?, quant=?,
		min_compute_capability=?, valid_tp=?, kv_bytes_per_token=?, kv_replicated_across_tp=?, runtime_overhead_bytes=?,
		max_model_len=?, min_context_tokens=?, engine=?, engine_image=?, min_driver_version=?, engine_args=?, metrics_map=?,
		startup_timeout_s=?, license=?, commercial_use=?
		WHERE id=? AND status='draft'`,
		e.ModelKey, e.DisplayName, e.Source, e.Repo, e.Revision, []byte(e.Manifest), e.WeightsBytes, e.Quant,
		e.MinComputeCapability, e.ValidTP, e.KVBytesPerToken, e.KVReplicatedAcrossTP, e.RuntimeOverheadBytes,
		e.MaxModelLen, e.MinContextTokens, e.Engine, e.EngineImage, e.MinDriverVersion,
		[]byte(e.EngineArgs), []byte(e.MetricsMap), e.StartupTimeoutS, e.License, e.CommercialUse, id)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func transitionTx(tx *sqlx.Tx, id int64, to string, from ...string) (int64, error) {
	q, args := "UPDATE model_catalog SET status=? WHERE id=? AND status IN (", []any{to, id}
	for i, f := range from {
		if i > 0 {
			q += ","
		}
		q += "?"
		args = append(args, f)
	}
	res, err := tx.Exec(q+")", args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func hasPassVerificationTx(tx *sqlx.Tx, modelID int64) (bool, error) {
	var n int
	err := tx.Get(&n, "SELECT COUNT(*) FROM model_catalog_verifications WHERE model_id=? AND result='pass'", modelID)
	return n > 0, err
}

func insertVerificationTx(tx *sqlx.Tx, v *Verification) error {
	res, err := tx.Exec(`INSERT INTO model_catalog_verifications
		(model_id, gpu_model, cards, tp, max_model_len, measured_overhead_bytes, result, notes, tested_by, tested_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		v.ModelID, v.GPUModel, v.Cards, v.TP, v.MaxModelLen, v.MeasuredOverheadBytes, v.Result, v.Notes, v.TestedBy, v.TestedAt)
	if err != nil {
		return err
	}
	v.ID, _ = res.LastInsertId()
	return nil
}

func (r *Repository) ListVerifications(modelID int64) ([]Verification, error) {
	list := []Verification{}
	err := r.db.Select(&list, "SELECT "+verificationColumns+
		" FROM model_catalog_verifications WHERE model_id=? ORDER BY tested_at DESC, id DESC", modelID)
	return list, err
}

// PassedGPUModels 每个条目有 result=pass 验证记录的卡型(F8 与 GET /models 的"已验证卡型")。
func (r *Repository) PassedGPUModels(modelIDs []int64) (map[int64][]string, error) {
	out := map[int64][]string{}
	if len(modelIDs) == 0 {
		return out, nil
	}
	q, args, err := sqlx.In(`SELECT DISTINCT model_id, gpu_model FROM model_catalog_verifications
		WHERE result='pass' AND model_id IN (?) ORDER BY model_id, gpu_model`, modelIDs)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ModelID  int64  `db:"model_id"`
		GPUModel string `db:"gpu_model"`
	}
	if err := r.db.Select(&rows, r.db.Rebind(q), args...); err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.ModelID] = append(out[row.ModelID], row.GPUModel)
	}
	return out, nil
}

func (r *Repository) beginTx() (*sqlx.Tx, error) { return r.db.Beginx() }
