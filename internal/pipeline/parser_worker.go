package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fuadarradhi/uuparser/internal/localllm"
	"github.com/fuadarradhi/uuparser/internal/logx"
	"github.com/fuadarradhi/uuparser/internal/parser"
	"github.com/fuadarradhi/uuparser/internal/store"
)

const parserIdleInterval = 15 * time.Second

func parserWorker(ctx context.Context, deps Deps) {
	// Jaring pengaman: dokumen yang macet di status 'parsing' (proses mati
	// di tengah parse) tidak akan pernah terambil lagi oleh ClaimForParse
	// (yang mencari status='ocr_done') tanpa ini. Lihat RequeueStuckParsing.
	if n, err := deps.Store.RequeueStuckParsing(ctx); err != nil {
		logx.Warn("parser: requeue status 'parsing' macet gagal: %v", err)
	} else if n > 0 {
		logx.Info("parser: %d dokumen berstatus 'parsing' macet, dikembalikan ke 'ocr_done'", n)
	}
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

	res, err := parser.ParseAllowNonRegulation(pages, job.Jenis)
	var status string
	var reportJSON, notesJSON []byte
	var nodeRows []store.NodeInsert
	// flags (2026-07-24): lihat catatan store.ReviewFlagInsert. Tetap nil
	// di jalur Parse() gagal di bawah — tidak ada apa pun untuk ditinjau
	// bila parse sendiri tidak menghasilkan apa-apa.
	var flags []store.ReviewFlagInsert

	if err != nil {
		status = "FAIL"
		reportJSON, _ = json.Marshal(map[string]string{"error": err.Error()})
		notesJSON = []byte("[]")
		// [Diperbaiki 2026-07-24] SEBELUMNYA tidak ada apa pun ditulis ke
		// debug saat Parse() gagal — dokumen yang ditolak gerbang
		// deterministik parser (mis. Surat Edaran tanpa Pasal/BAB/
		// Menimbang/Mengingat/Memutuskan/Menetapkan sama sekali) jadi
		// TIDAK PUNYA parse.txt sama sekali, seolah tahap parse belum
		// pernah jalan — padahal sudah, cuma gagal. Sekarang alasannya
		// tetap ditulis supaya terlihat, bukan hilang.
		if deps.DebugResult {
			var b strings.Builder
			fmt.Fprintf(&b, "=== PARSE GAGAL — dokumen %d ===\n\nError: %v\n", job.ID, err)
			if errors.Is(err, parser.ErrNotLegalDocument) {
				b.WriteString("\nGerbang deterministik parser (internal/parser/classify.go) TIDAK " +
					"menemukan penanda struktur hukum formal (Pasal / Diktum bernomor kata / " +
					"BAB+konsiderans / Menimbang-Mengingat-Memperhatikan-Memutuskan-Menetapkan) " +
					"di SELURUH halaman dokumen ini.\n\n" +
					"Ini gerbang TERPISAH dari klasifikasi tahap OCR (yang menerima dokumen ini " +
					"berdasarkan penilaian model, bukan pola deterministik) — dokumen bisa saja " +
					"lolos tahap OCR (ocr.txt ada) tapi tetap gagal di sini kalau tidak memuat " +
					"satu pun penanda di atas. Sering terjadi pada Surat Edaran/Instruksi yang " +
					"formatnya cuma poin bernomor biasa, tanpa konsiderans formal sama sekali.\n")
			}
			tulisDebugParse(deps.DebugDir, job.ID, b.String())
		}
	} else {
		rep := parser.Diagnose(res)
		status = string(rep.Status)
		reportJSON, _ = json.Marshal(rep)
		notes := make([]string, 0)
		for _, w := range res.DocumentWarnings {
			notes = append(notes, w.Message)
		}

		// review_flags dari Diagnose (2026-07-24) — SETIAP Issue jadi satu
		// baris level-dokumen (parser.Issue tidak membawa referensi node).
		// Ini mekanisme UI yang diminta user: satu tabel sederhana untuk
		// "apa saja yang perlu ditinjau", tanpa UI perlu mem-parse JSON
		// report sendiri.
		for _, is := range rep.Issues {
			flags = append(flags, store.ReviewFlagInsert{
				NodeIdx: -1, Source: "diagnose", Code: is.Code,
				Severity: string(is.Severity), Message: is.Message,
			})
		}
		// review_flags dari Node.Warnings — node-level (NodeIdx = indeks di
		// res.Nodes, dipetakan ke id asli oleh InsertParseResult).
		for i, n := range res.Nodes {
			for _, w := range n.Warnings {
				flags = append(flags, store.ReviewFlagInsert{
					NodeIdx: i, Source: "parser_warning", Code: "NODE_WARNING",
					Severity: string(w.Severity), Message: w.Message,
				})
			}
		}

		// tinjauanDebug menampung debug TEKS gabungan dari KEDUA jenis
		// tinjauan model di bawah (ANCHOR_LEAK & teks yatim) — SATU
		// builder bersama, ditulis SEKALI ke tinjauan.txt di akhir.
		// [Diperbaiki 2026-07-24] Sebelumnya masing-masing blok menulis
		// berkas sendiri-sendiri lewat tulisDebugTinjauan (yang MENIMPA,
		// bukan menambah) — begitu blok teks-yatim ditambahkan, ia diam-
		// diam menimpa hasil blok ANCHOR_LEAK yang jalan lebih dulu.
		var tinjauanDebug strings.Builder

		// Tinjauan model (2026-07-23) — "parser bermasalah, ada kecerdasan
		// untuk memanggil model teks" (permintaan user): dipanggil HANYA
		// untuk node yang PARSER SENDIRI curigai lewat sinyal
		// parser.AnchorLeakNodes (lihat diagnose.go/thinking.go) — BUKAN
		// tiap dokumen, BUKAN tiap halaman. Jawabannya TIDAK PERNAH
		// mengubah nodeRows/database — murni ditambahkan ke
		// extraction_notes sebagai bahan tinjauan manusia, konsisten
		// dengan prinsip "model membaca, kode menyimpulkan" di seluruh
		// pipeline ini (di sini malah lebih ketat: kode pun tidak
		// menyimpulkan apa-apa dari jawabannya).
		if idxs := parser.AnchorLeakNodes(res); len(idxs) > 0 && deps.Text != nil {
			if deps.DebugResult {
				tinjauanDebug.WriteString("--- Dipicu ANCHOR_LEAK ---\n\n")
			}
			for _, idx := range idxs {
				n := res.Nodes[idx]
				logx.Info("dokumen %d: menjalankan tinjauan model untuk node [%s/%s] hal %d-%d (ANCHOR_LEAK)",
					job.ID, n.Section, n.NodeType, n.StartPage, n.EndPage)
				bermasalah, penjelasan, rawTinjau, terr := AskTinjauan(
					ctx, deps.Text, deps.Prompts.Tinjau, n.Text, localllm.TextParams{})
				if deps.DebugResult {
					fmt.Fprintf(&tinjauanDebug, "=== NODE [%s/%s] hal %d-%d ===\n--- TEKS ---\n%s\n\n--- JAWABAN MENTAH MODEL ---\n%s\n",
						n.Section, n.NodeType, n.StartPage, n.EndPage, n.Text, rawTinjau)
					if terr != nil {
						fmt.Fprintf(&tinjauanDebug, "--- GAGAL DIURAI: %v ---\n", terr)
					}
					tinjauanDebug.WriteString("\n")
				}
				if terr != nil {
					logx.Warn("dokumen %d: tinjauan model gagal untuk node [%s/%s]: %v",
						job.ID, n.Section, n.NodeType, terr)
					continue
				}
				// HANYA dicatat kalau model memang setuju ada yang
				// tercampur — jawaban bermasalah=false tidak perlu
				// membanjiri extraction_notes dengan "sudah dicek, aman".
				if bermasalah {
					notes = append(notes, fmt.Sprintf(
						"Tinjauan model (node [%s/%s], hal %d-%d): %s",
						n.Section, n.NodeType, n.StartPage, n.EndPage, penjelasan))
					flags = append(flags, store.ReviewFlagInsert{
						NodeIdx: idx, Source: "model_anchor_leak", Code: "ANCHOR_LEAK",
						Severity: string(parser.SeverityNeedsReview), Message: penjelasan,
					})
				}
			}
		}

		// Tinjauan model untuk teks YATIM (2026-07-24) — sinyal BERBEDA
		// dari ANCHOR_LEAK di atas: bukan "penanda section di tengah
		// teks", tapi "sepotong teks gagal dicocokkan ke struktur apa
		// pun sama sekali" (parser.OrphanWarningNodes, lihat diagnose.go).
		// Pengurai TAHU ada yang tidak cocok (makanya sudah jadi
		// NODE_WARNINGS) tapi tidak tahu apakah itu artefak halaman yang
		// aman diabaikan atau isi sungguhan yang butuh rumah sendiri —
		// persis kelas keputusan regex yang disepakati minta bantuan
		// thinking.gguf. Memakai fungsi AskTinjauan yang SAMA (bentuk
		// jawaban {bermasalah, penjelasan} identik), hanya prompt-nya
		// beda (prompts/orphan.md) dan teks yang dikirim menggabungkan
		// TEKS NODE + setiap TEKS YATIM yang menempel, supaya model
		// menilai keduanya sekaligus, bukan potongan yatim sendirian
		// tanpa konteks. Prinsip sama persis: jawabannya TIDAK PERNAH
		// mengubah nodeRows/database, murni catatan tambahan.
		if idxs := parser.OrphanWarningNodes(res); len(idxs) > 0 && deps.Text != nil {
			if deps.DebugResult {
				if tinjauanDebug.Len() > 0 {
					tinjauanDebug.WriteString("\n")
				}
				tinjauanDebug.WriteString("--- Dipicu teks yatim ---\n\n")
			}
			for _, idx := range idxs {
				n := res.Nodes[idx]
				var gabungan strings.Builder
				fmt.Fprintf(&gabungan, "TEKS NODE:\n%s\n", n.Text)
				for _, w := range n.Warnings {
					if w.OrphanText != nil {
						fmt.Fprintf(&gabungan, "\nTEKS YATIM (posisi: %s):\n%s\n", w.Position, *w.OrphanText)
					}
				}
				logx.Info("dokumen %d: menjalankan tinjauan model untuk node [%s/%s] hal %d-%d (teks yatim)",
					job.ID, n.Section, n.NodeType, n.StartPage, n.EndPage)
				bermasalah, penjelasan, rawTinjau, terr := AskTinjauan(
					ctx, deps.Text, deps.Prompts.OrphanReview, gabungan.String(), localllm.TextParams{})
				if deps.DebugResult {
					fmt.Fprintf(&tinjauanDebug, "=== NODE [%s/%s] hal %d-%d ===\n--- TEKS DIKIRIM ---\n%s\n--- JAWABAN MENTAH MODEL ---\n%s\n",
						n.Section, n.NodeType, n.StartPage, n.EndPage, gabungan.String(), rawTinjau)
					if terr != nil {
						fmt.Fprintf(&tinjauanDebug, "--- GAGAL DIURAI: %v ---\n", terr)
					}
					tinjauanDebug.WriteString("\n")
				}
				if terr != nil {
					logx.Warn("dokumen %d: tinjauan model (teks yatim) gagal untuk node [%s/%s]: %v",
						job.ID, n.Section, n.NodeType, terr)
					continue
				}
				// HANYA dicatat kalau model menilai teks yatim ini
				// benar-benar isi sungguhan yang butuh rumah — bukan
				// artefak yang aman diabaikan (bermasalah=false tidak
				// perlu membanjiri extraction_notes).
				if bermasalah {
					notes = append(notes, fmt.Sprintf(
						"Tinjauan model — teks yatim (node [%s/%s], hal %d-%d): %s",
						n.Section, n.NodeType, n.StartPage, n.EndPage, penjelasan))
					flags = append(flags, store.ReviewFlagInsert{
						NodeIdx: idx, Source: "model_orphan_review", Code: "ORPHAN_REVIEW",
						Severity: string(parser.SeverityNeedsReview), Message: penjelasan,
					})
				}
			}
		}

		if deps.DebugResult && tinjauanDebug.Len() > 0 {
			tulisDebugTinjauan(deps.DebugDir, job.ID, tinjauanDebug.String())
		}

		notesJSON, _ = json.Marshal(notes)
		nodeRows = mapNodesToInserts(res.Nodes)
		if deps.DebugResult {
			tulisDebugParse(deps.DebugDir, job.ID, formatNodesUntukDebug(status, res.Nodes))
			// parse_tree.json: pohon parent-child eksplisit dari nodeRows
			// yang sama persis dikirim ke InsertParseResult di bawah —
			// lihat catatan di buildDebugTree (debug_writer.go).
			tulisDebugParseTree(deps.DebugDir, job.ID, formatNodeTreeJSON(status, nodeRows))
		}
	}
	if err := st.InsertParseResult(ctx, job.ID, status, reportJSON, notesJSON, nodeRows, flags); err != nil {
		logx.Fail(fmt.Sprintf("dokumen %d", job.ID), "simpan hasil parse: %v", err)
		return
	}
	logx.OK("parse selesai · dokumen %d · status=%s · %d node · %d flag tinjauan", job.ID, status, len(nodeRows), len(flags))
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
			// Diktum TIDAK butuh field terpisah di sini: Label (di bawah,
			// lewat ownLevelLabel) sudah membawa "KESATU"/"KEDUA"/dst untuk
			// node_type=diktum, dan trigger DB nodes_recompute_own_labels
			// menurunkan diktum_number darinya — pola identik pasal_number/
			// ayat_number, tidak perlu kolom Go tambahan.
			// Label WAJIB diisi dari field level node itu sendiri: trigger
			// BEFORE INSERT di DB (nodes_recompute_own_labels) menghitung
			// bab_number/bagian_label/paragraf_label/pasal_number dari
			// NEW.label — tanpa ini semua label flat jadi NULL saat insert.
			Label:   ownLevelLabel(n),
			Content: n.Text, StartPage: n.StartPage, EndPage: n.EndPage,
			OrderIndex: int64(n.DocOrder),
			IsAppendix: n.IsAppendix,
			IsDictum:   n.IsDictum,
			IsTitle:    n.IsTitle,
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
	case parser.NodeDiktum:
		return n.Diktum
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
