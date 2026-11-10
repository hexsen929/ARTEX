package db

import (
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/Autumn-27/artex/knowledge"
)

func restoreKnowledgeArchive(tx *sql.Tx, snapshot *TaskArchiveSnapshot) error {
	if snapshot.FormatVersion < 7 {
		return nil
	}
	raw, exists := snapshot.Tables["knowledge_docs"]
	if !exists {
		return errors.New("archive missing reference documents")
	}
	rows, err := decodeArchiveRows(raw)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`SELECT pg_advisory_xact_lock(7337741025)`); err != nil {
		return err
	}
	var count, size, chunks int
	if err = tx.QueryRow(`SELECT count(*),COALESCE(sum(octet_length(content)),0),COALESCE(sum(chunks),0) FROM knowledge_docs`).Scan(&count, &size, &chunks); err != nil {
		return err
	}
	for _, row := range rows {
		task, ok := jsonInt64(row["task_id"])
		if !ok || task != snapshot.TaskID {
			return errors.New("archived document crosses task ownership")
		}
		content, _ := row["content"].(string)
		title, _ := row["title"].(string)
		category, _ := row["category"].(string)
		filename, _ := row["file_name"].(string)
		doc := knowledge.Document{TaskID: task, Title: title, Category: category, Content: content, FileName: filename}
		if err := doc.Validate(); err != nil {
			return err
		}
		parts, err := knowledge.Chunk(content)
		if err != nil {
			return err
		}
		count++
		size += len(content)
		chunks += len(parts)
		row["indexed_epoch"] = 0
		row["chunks"] = len(parts)
	}
	if count > knowledge.MaxDocuments || size > knowledge.MaxLibraryBytes || chunks > knowledge.MaxLibraryChunks {
		return knowledge.ErrQuota
	}
	encoded, _ := json.Marshal(rows)
	if err = insertArchiveRows(tx, "knowledge_docs", encoded); err != nil {
		return err
	}
	// Derive chunks from the saved bounded text; never trust imported embeddings
	// or chunk rows to point at another task's document.
	for _, row := range rows {
		id, ok := jsonInt64(row["id"])
		if !ok {
			return knowledge.ErrInput
		}
		parts, _ := knowledge.Chunk(row["content"].(string))
		for i, text := range parts {
			if _, err = tx.Exec(`INSERT INTO knowledge_chunks(doc_id,chunk_index,content) VALUES($1,$2,$3)`, id, i, text); err != nil {
				return err
			}
		}
	}
	_, err = tx.Exec(`SELECT setval(pg_get_serial_sequence('knowledge_docs','id'),GREATEST(COALESCE((SELECT max(id) FROM knowledge_docs),0),1),true)`)
	return err
}
