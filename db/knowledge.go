package db

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/knowledge"
)

func (d *DB) KnowledgeConfig(ctx context.Context) (knowledge.Config, error) {
	var c knowledge.Config
	err := d.QueryRowContext(ctx, `SELECT enabled,base_url,model,dimension,api_key,epoch,to_regclass('knowledge_vectors') IS NOT NULL FROM knowledge_config WHERE singleton`).Scan(&c.Enabled, &c.BaseURL, &c.Model, &c.Dimension, &c.APIKey, &c.Epoch, &c.VectorAvailable)
	c.HasKey = c.APIKey != ""
	return c, err
}

func (d *DB) SaveKnowledgeConfig(ctx context.Context, c knowledge.Config, key *string) (knowledge.Config, error) {
	if key != nil {
		c.APIKey = *key
	}
	if err := c.Validate(); err != nil {
		return c, err
	}
	var value any
	if key != nil {
		value = *key
	}
	result, err := d.ExecContext(ctx, `UPDATE knowledge_config SET enabled=$1,base_url=$2,model=$3,dimension=$4,api_key=COALESCE($5,api_key),epoch=epoch+1 WHERE singleton AND epoch=$6`, c.Enabled, c.BaseURL, c.Model, c.Dimension, value, c.Epoch)
	if err != nil {
		return c, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return c, knowledge.ErrConflict
	}
	return d.KnowledgeConfig(ctx)
}

func liveKnowledgeTask(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, taskID int64) error {
	if taskID == 0 {
		return nil
	}
	if taskID < 0 {
		return knowledge.ErrInput
	}
	var alive bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1 AND deleted_at IS NULL AND archived_at IS NULL)`, taskID).Scan(&alive)
	if err != nil {
		return err
	}
	if !alive {
		return sql.ErrNoRows
	}
	return nil
}

func (d *DB) SaveKnowledgeDocument(ctx context.Context, doc knowledge.Document) (*knowledge.Document, error) {
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	chunks, err := knowledge.Chunk(doc.Content)
	if err != nil {
		return nil, err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(7337741025)`); err != nil {
		return nil, err
	}
	if doc.TaskID > 0 {
		var id int64
		if err = tx.QueryRowContext(ctx, `SELECT id FROM tasks WHERE id=$1 AND deleted_at IS NULL AND archived_at IS NULL FOR SHARE`, doc.TaskID).Scan(&id); err != nil {
			return nil, err
		}
	}
	if doc.ID > 0 {
		var version, taskID int64
		if err = tx.QueryRowContext(ctx, `SELECT version,COALESCE(task_id,0) FROM knowledge_docs WHERE id=$1 FOR UPDATE`, doc.ID).Scan(&version, &taskID); err != nil {
			return nil, err
		}
		if version != doc.Version || taskID != doc.TaskID {
			return nil, knowledge.ErrConflict
		}
	}
	var count, size, totalChunks int
	if err = tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(octet_length(content)),0),COALESCE(sum(chunks),0) FROM knowledge_docs WHERE id<>$1`, doc.ID).Scan(&count, &size, &totalChunks); err != nil {
		return nil, err
	}
	if count >= knowledge.MaxDocuments || size+len(doc.Content) > knowledge.MaxLibraryBytes || totalChunks+len(chunks) > knowledge.MaxLibraryChunks {
		return nil, knowledge.ErrQuota
	}
	if doc.ID == 0 {
		err = tx.QueryRowContext(ctx, `INSERT INTO knowledge_docs(task_id,title,category,content,file_name,chunks) VALUES(NULLIF($1,0),$2,$3,$4,$5,$6) RETURNING id,version`, doc.TaskID, doc.Title, doc.Category, doc.Content, doc.FileName, len(chunks)).Scan(&doc.ID, &doc.Version)
	} else {
		err = tx.QueryRowContext(ctx, `UPDATE knowledge_docs SET title=$2,category=$3,content=$4,file_name=$5,chunks=$6,version=version+1,indexed_epoch=0,updated_at=now() WHERE id=$1 RETURNING version`, doc.ID, doc.Title, doc.Category, doc.Content, doc.FileName, len(chunks)).Scan(&doc.Version)
	}
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM knowledge_chunks WHERE doc_id=$1`, doc.ID); err != nil {
		return nil, err
	}
	for i, chunk := range chunks {
		if _, err = tx.ExecContext(ctx, `INSERT INTO knowledge_chunks(doc_id,chunk_index,content) VALUES($1,$2,$3)`, doc.ID, i, chunk); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return d.KnowledgeDocument(ctx, doc.ID)
}

const knowledgeCols = `d.id,COALESCE(d.task_id,0),d.title,d.category,d.content,d.file_name,d.version,octet_length(d.content),d.chunks,(d.indexed_epoch=c.epoch AND c.enabled)`

func scanKnowledge(row interface{ Scan(...any) error }) (*knowledge.Document, error) {
	var doc knowledge.Document
	err := row.Scan(&doc.ID, &doc.TaskID, &doc.Title, &doc.Category, &doc.Content, &doc.FileName, &doc.Version, &doc.Bytes, &doc.Chunks, &doc.Indexed)
	return &doc, err
}
func (d *DB) KnowledgeDocument(ctx context.Context, id int64) (*knowledge.Document, error) {
	return scanKnowledge(d.QueryRowContext(ctx, `SELECT `+knowledgeCols+` FROM knowledge_docs d CROSS JOIN knowledge_config c WHERE d.id=$1`, id))
}

func (d *DB) KnowledgeDocuments(ctx context.Context, taskID int64, all bool) ([]knowledge.Document, error) {
	if err := liveKnowledgeTask(ctx, d, taskID); err != nil {
		return nil, err
	}
	rows, err := d.QueryContext(ctx, `SELECT `+strings.Replace(knowledgeCols, "d.content,", "'',", 1)+` FROM knowledge_docs d CROSS JOIN knowledge_config c WHERE $2 OR d.task_id IS NULL OR d.task_id=$1 ORDER BY d.id DESC LIMIT 256`, taskID, all)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []knowledge.Document{}
	for rows.Next() {
		doc, err := scanKnowledge(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *doc)
	}
	return out, rows.Err()
}

func (d *DB) DeleteKnowledgeDocument(ctx context.Context, id, version int64) error {
	res, err := d.ExecContext(ctx, `DELETE FROM knowledge_docs WHERE id=$1 AND version=$2`, id, version)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return knowledge.ErrConflict
	}
	return nil
}

func (d *DB) KnowledgeChunks(ctx context.Context, id int64) ([]string, error) {
	rows, err := d.QueryContext(ctx, `SELECT content FROM knowledge_chunks WHERE doc_id=$1 ORDER BY chunk_index`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func vectorLiteral(v []float32) string {
	b := make([]string, len(v))
	for i, x := range v {
		b[i] = strconv.FormatFloat(float64(x), 'g', -1, 32)
	}
	return "[" + strings.Join(b, ",") + "]"
}

// Index publication checks both the document version and embedding settings.
// An edit/deletion/model change cannot publish a stale vector snapshot.
func (d *DB) PublishKnowledgeVectors(ctx context.Context, doc knowledge.Document, c knowledge.Config, vecs [][]float32) error {
	if len(vecs) != doc.Chunks {
		return knowledge.ErrInput
	}
	for _, v := range vecs {
		if !knowledge.ValidVector(v, c.Dimension) {
			return knowledge.ErrInput
		}
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var epoch, version int64
	var enabled bool
	if err = tx.QueryRowContext(ctx, `SELECT epoch,enabled FROM knowledge_config WHERE singleton FOR SHARE`).Scan(&epoch, &enabled); err != nil {
		return err
	}
	if epoch != c.Epoch || !enabled {
		return knowledge.ErrConflict
	}
	if err = tx.QueryRowContext(ctx, `SELECT version FROM knowledge_docs WHERE id=$1 FOR UPDATE`, doc.ID).Scan(&version); err != nil {
		return err
	}
	if version != doc.Version {
		return knowledge.ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM knowledge_vectors WHERE doc_id=$1`, doc.ID); err != nil {
		return err
	}
	for i, v := range vecs {
		if _, err = tx.ExecContext(ctx, `INSERT INTO knowledge_vectors(doc_id,chunk_index,epoch,embedding) VALUES($1,$2,$3,$4::vector)`, doc.ID, i, c.Epoch, vectorLiteral(v)); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE knowledge_docs SET indexed_epoch=$2 WHERE id=$1`, doc.ID, c.Epoch); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) SearchKnowledge(ctx context.Context, taskID int64, query string, limit int, c knowledge.Config, vector []float32) ([]knowledge.Hit, error) {
	if taskID < 0 || strings.TrimSpace(query) == "" || !knowledge.ValidText(query, 1000) || limit < 1 || limit > 8 {
		return nil, knowledge.ErrInput
	}
	if err := liveKnowledgeTask(ctx, d, taskID); err != nil {
		return nil, err
	}
	where := ` (d.task_id IS NULL OR d.task_id=$1) AND (d.task_id IS NULL OR EXISTS(SELECT 1 FROM tasks t WHERE t.id=d.task_id AND t.deleted_at IS NULL AND t.archived_at IS NULL))`
	args := []any{taskID}
	score := "0"
	join := ""
	clauses := []string{}
	if vector != nil {
		if !knowledge.ValidVector(vector, c.Dimension) {
			return nil, knowledge.ErrInput
		}
		args = append(args, c.Epoch, vectorLiteral(vector))
		join = ` JOIN knowledge_vectors v ON v.doc_id=k.doc_id AND v.chunk_index=k.chunk_index CROSS JOIN knowledge_config cfg`
		where += ` AND v.epoch=$2 AND d.indexed_epoch=$2 AND cfg.epoch=$2 AND cfg.enabled AND vector_dims(v.embedding)=` + strconv.Itoa(c.Dimension)
		score = `1-(v.embedding <=> $3::vector)`
	} else {
		terms := strings.Fields(strings.TrimSpace(query))
		if len(terms) > 8 {
			terms = terms[:8]
		}
		scores := []string{}
		for _, term := range terms {
			args = append(args, memoryLikePattern(term))
			n := len(args)
			expr := fmt.Sprintf(`(d.title ILIKE $%[1]d OR d.category ILIKE $%[1]d OR k.content ILIKE $%[1]d)`, n)
			clauses = append(clauses, expr)
			scores = append(scores, `CASE WHEN `+expr+` THEN 1 ELSE 0 END`)
		}
		where += ` AND (` + strings.Join(clauses, ` OR `) + `)`
		score = strings.Join(scores, "+")
	}
	args = append(args, limit)
	rows, err := d.QueryContext(ctx, `SELECT d.id,d.version,k.chunk_index,COALESCE(d.task_id,0),d.title,k.content,`+score+` AS score FROM knowledge_chunks k JOIN knowledge_docs d ON d.id=k.doc_id`+join+` WHERE `+where+` ORDER BY score DESC,d.id DESC,k.chunk_index LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []knowledge.Hit{}
	for rows.Next() {
		var h knowledge.Hit
		if err = rows.Scan(&h.DocID, &h.Version, &h.Chunk, &h.TaskID, &h.Title, &h.Content, &h.Score); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Counters contain no query text or document bodies and cannot grow per search.
func (d *DB) CountKnowledgeRetrieval(ctx context.Context, hits []knowledge.Hit) {
	for _, h := range hits {
		_, _ = d.ExecContext(ctx, `UPDATE knowledge_docs SET retrievals=retrievals+1 WHERE id=$1 AND version=$2`, h.DocID, h.Version)
	}
}
