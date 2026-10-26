package videojob

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// SQLStore keeps jobs in the video_jobs table from migrations/001. The caller
// opens *sql.DB with the site's Postgres driver.
type SQLStore struct{ DB *sql.DB }

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *SQLStore) row(j *VideoJob) ([]any, error) {
	doc, err := json.Marshal(j)
	if err != nil {
		return nil, err
	}
	var provider, model, task string
	if cur := j.Current(); cur != nil {
		provider, model, task = cur.Provider, cur.Model, cur.ExternalTaskID
	}
	return []any{j.ID, j.IdempotencyKey, j.Revision, string(j.Status), j.Request.EpisodeID, j.Request.ShotID, string(j.Request.InputMode),
		nullable(provider), nullable(model), nullable(task), j.TotalCostMicroUSD(), j.CostCapMicroUSD, nullable(j.LegacyVideoGenerationID), doc, j.CreatedAt, j.UpdatedAt}, nil
}

func (s *SQLStore) CreateJob(ctx context.Context, j *VideoJob) (*VideoJob, bool, error) {
	args, err := s.row(j)
	if err != nil {
		return nil, false, err
	}
	res, err := s.DB.ExecContext(ctx, `INSERT INTO video_jobs (id, idempotency_key, revision, status, episode_id, shot_id, input_mode,
		current_provider, current_model, current_external_task_id, total_cost_micro_usd, cost_cap_micro_usd, legacy_video_generation_id, doc, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) ON CONFLICT (idempotency_key) DO NOTHING`, args...)
	if err != nil {
		return nil, false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return j, true, nil
	}
	existing, err := s.get(ctx, `SELECT doc, revision FROM video_jobs WHERE idempotency_key = $1`, j.IdempotencyKey)
	return existing, false, err
}

func (s *SQLStore) get(ctx context.Context, query, arg string) (*VideoJob, error) {
	var doc []byte
	var revision int
	err := s.DB.QueryRowContext(ctx, query, arg).Scan(&doc, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var j VideoJob
	if err := json.Unmarshal(doc, &j); err != nil {
		return nil, err
	}
	j.Revision = revision
	return &j, nil
}

func (s *SQLStore) GetJob(ctx context.Context, id string) (*VideoJob, error) {
	return s.get(ctx, `SELECT doc, revision FROM video_jobs WHERE id = $1`, id)
}

// SaveJob writes the job if nobody saved it since it was read.
func (s *SQLStore) SaveJob(ctx context.Context, j *VideoJob) error {
	expected := j.Revision
	j.Revision++
	args, err := s.row(j)
	if err == nil {
		var res sql.Result
		res, err = s.DB.ExecContext(ctx, `UPDATE video_jobs SET revision=$2, status=$3, current_provider=$4, current_model=$5,
			current_external_task_id=$6, total_cost_micro_usd=$7, legacy_video_generation_id=$8, doc=$9, updated_at=$10
			WHERE id=$1 AND revision=$11`,
			j.ID, j.Revision, args[3], args[7], args[8], args[9], args[10], args[12], args[13], args[15], expected)
		if err == nil {
			if n, _ := res.RowsAffected(); n != 1 {
				err = ErrConflict
				if _, getErr := s.GetJob(ctx, j.ID); errors.Is(getErr, ErrNotFound) {
					err = ErrNotFound
				}
			}
		}
	}
	if err != nil {
		j.Revision = expected
	}
	return err
}
