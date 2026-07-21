package pipeline

import (
	"context"

	"github.com/fuadarradhi/uuparser/internal/store"
)

// dbPageStore mengadaptasi *store.Store (Postgres) ke interface kecil
// extractor.PageStore, supaya package extractor sendiri tidak perlu tahu
// apa-apa soal DB — lihat internal/extractor/extractor.go.
type dbPageStore struct {
	st         *store.Store
	documentID string
}

func (d dbPageStore) HasPage(ctx context.Context, page int) (bool, error) {
	return d.st.HasPage(ctx, d.documentID, page)
}

func (d dbPageStore) SavePage(ctx context.Context, page int, text string, isEmpty, isTruncated bool, inkRatio, croppedPct float64, durationMS int, notes []string) error {
	return d.st.SavePage(ctx, d.documentID, page, text, isEmpty, isTruncated, inkRatio, croppedPct, notes, durationMS)
}

func (d dbPageStore) ReadPages(ctx context.Context, a, b int) ([]string, error) {
	return d.st.ReadPageRange(ctx, d.documentID, a, b)
}
