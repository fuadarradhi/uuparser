package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/fuadarradhi/uuparser/internal/logx"
	"github.com/fuadarradhi/uuparser/internal/parser"
	"github.com/fuadarradhi/uuparser/internal/store"
)

const parserIdleInterval = 15 * time.Second

func parserWorker(ctx context.Context, deps Deps) {
	for {
		if ctx.Err() != nil {
			return
		}
		processed := drainParse(ctx, deps)
		if !processed {
			select {
			case <-ctx.Done():
				return
			case <-time.After(parserIdleInterval):
			}
		}
	}
}

func drainParse(ctx context.Context, deps Deps) (processedAny bool) {
	st := deps.Store
	for {
		if ctx.Err() != nil {
			return processedAny
		}
		job, err := st.ClaimForParse(ctx)
		if err == store.ErrNoWork {
			return processedAny
		}
		if err != nil {
			logx.Warn("parser: klaim gagal: %v", err)
			return processedAny
		}
		processedAny = true
		processOneParse(ctx, deps, job)
	}
}

func processOneParse(ctx context.Context, deps Deps, job store.ParseJob) {
	st := deps.Store
	pages := make([]string, 0, job.NumPages)
	for i := 1; i <= job.NumPages; i++ {
		text, err := st.GetPageText(ctx, job.ID, i)
		if err != nil {
			text = ""
		}
		pages = append(pages, text)
	}

	res, err := parser.Parse(pages)
	var status string
	var reportJSON, notesJSON []byte
	var nodeRows []store.NodeInsert

	if err != nil {
		status = "FAIL"
		reportJSON, _ = json.Marshal(map[string]string{"error": err.Error()})
		notesJSON = []byte("[]")
	} else {
		rep := parser.Diagnose(res)
		status = string(rep.Status)
		reportJSON, _ = json.Marshal(rep)
		notes := make([]string, 0)
		for _, w := range res.DocumentWarnings {
			notes = append(notes, w.Message)
		}
		notesJSON, _ = json.Marshal(notes)
		nodeRows = mapNodesToInserts(res.Nodes)
		if deps.DebugResult {
			tulisDebugParse(deps.DataDir, job.ID, formatNodesUntukDebug(status, res.Nodes))
		}
	}

	if err := st.InsertParseResult(ctx, job.ID, status, reportJSON, notesJSON, nodeRows); err != nil {
		logx.Fail(fmt.Sprintf("dokumen %d", job.ID), "simpan hasil parse: %v", err)
		return
	}
	logx.OK("parse selesai · dokumen %d · status=%s · %d node", job.ID, status, len(nodeRows))
}

// mapNodesToInserts memetakan parser.Node (flat, per-baris, konteks label
// via pointer) menjadi store.NodeInsert (flat juga, tapi dengan ParentIdx
// eksplisit) untuk di-insert sebagai pohon. parser.Node menyimpan label
// ANCESTOR PENUH (Bab/Bagian/Paragraf/Pasal/Ayat), bukan pointer parent — jadi
// pemetaan di sini memakai "parent = node terakhir satu tingkat di atas",
// yang valid selama urutan node linear sesuai DocOrder (dijamin parser.Parse).
// TERUJI lewat tes integrasi bertahap (multi-BAB, Pasal langsung di bawah BAB
// tanpa Bagian, penjelasan terpisah, invarian ParentIdx<index) — terhadap teks
// sintetis; dokumen OCR nyata pertama tetap perlu diperiksa hasilnya.
func mapNodesToInserts(nodes []parser.Node) []store.NodeInsert {
	out := make([]store.NodeInsert, 0, len(nodes))
	lastBab, lastBagian, lastParagraf, lastPasal := -1, -1, -1, -1
	prevSection := parser.Section("")

	for _, n := range nodes {
		// Ganti section = mulai pohon baru. Tanpa reset ini, Pasal di
		// penjelasan_pasal akan salah-parent ke BAB terakhir batang tubuh.
		if n.Section != prevSection {
			lastBab, lastBagian, lastParagraf, lastPasal = -1, -1, -1, -1
			prevSection = n.Section
		}

		parentIdx := -1
		switch n.NodeType {
		case parser.NodeBagian:
			parentIdx = lastBab
		case parser.NodeParagraf:
			parentIdx = pickParent(lastParagraf, lastBagian, lastBab)
		case parser.NodePasal:
			parentIdx = pickParent(lastParagraf, lastBagian, lastBab)
		case parser.NodeAyat:
			parentIdx = lastPasal
		}

		row := store.NodeInsert{
			ParentIdx: parentIdx,
			Section:   string(n.Section), NodeType: string(n.NodeType),
			BabNumber: n.Bab, BagianLabel: n.Bagian, ParagrafLabel: n.Paragraf,
			PasalNumber: n.Pasal, AyatNumber: n.Ayat, HurufLabel: n.Huruf, AngkaLabel: n.Angka,
			// Label WAJIB diisi dari field level node itu sendiri: trigger
			// BEFORE INSERT di DB (nodes_recompute_own_labels) menghitung
			// bab_number/bagian_label/paragraf_label/pasal_number dari
			// NEW.label — tanpa ini semua label flat jadi NULL saat insert.
			Label:   ownLevelLabel(n),
			Content: n.Text, StartPage: n.StartPage, EndPage: n.EndPage,
			OrderIndex: int64(n.DocOrder),
		}
		if warnBytes, err := json.Marshal(n.Warnings); err == nil {
			row.Warnings = warnBytes
		}
		out = append(out, row)

		idx := len(out) - 1
		switch n.NodeType {
		case parser.NodeBab:
			lastBab, lastBagian, lastParagraf, lastPasal = idx, -1, -1, -1
		case parser.NodeBagian:
			lastBagian, lastParagraf, lastPasal = idx, -1, -1
		case parser.NodeParagraf:
			lastParagraf, lastPasal = idx, -1
		case parser.NodePasal:
			lastPasal = idx
		}
	}
	return out
}

// ownLevelLabel mengembalikan label milik LEVEL node itu sendiri — nilai yang
// dipakai trigger DB sebagai sumber kolom flat levelnya ("BAB I", "1" untuk
// Pasal 1, "1"/"2" untuk Ayat). Node non-struktural (judul/item/paragraf_isi/
// penetapan) tidak punya label level → nil.
func ownLevelLabel(n parser.Node) *string {
	switch n.NodeType {
	case parser.NodeBab:
		return n.Bab
	case parser.NodeBagian:
		return n.Bagian
	case parser.NodeParagraf:
		return n.Paragraf
	case parser.NodePasal:
		return n.Pasal
	case parser.NodeAyat:
		return n.Ayat
	}
	return nil
}

func pickParent(candidates ...int) int {
	for _, c := range candidates {
		if c >= 0 {
			return c
		}
	}
	return -1
}
