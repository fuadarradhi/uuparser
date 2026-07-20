#!/bin/bash

show_help() {
cat << 'EOF'
Usage:
  ./catter.sh <source_dir> [options]

Description:
  Menggabungkan isi file dari folder dan menghasilkan output TERPISAH
  berdasarkan extension.

  Setiap extension akan menghasilkan file:
    catter_<ext>.txt

  Contoh:
    --ext go,html
    → catter_go.txt
    → catter_html.txt

Options:
  --ext <ext1,ext2,...>
      Filter berdasarkan extension file.
      Default: go

      Contoh:
        --ext html
        --ext go,js,html

  --exclude <dir1,dir2,...>
      Mengabaikan folder tertentu (recursive).

      Contoh:
        --exclude node_modules,oauth,tmp

  --max-size <size>
      Hanya ambil file di bawah ukuran tertentu.

      Format:
        k = kilobytes
        M = megabytes

      Contoh:
        --max-size 100k
        --max-size 2M

  -h, --help
      Menampilkan bantuan ini.

Examples:
  Default (hanya .go):
    ./catter.sh app/

  HTML saja:
    ./catter.sh app/ --ext html

  Multi extension:
    ./catter.sh app/ --ext go,js,html

  Exclude folder:
    ./catter.sh app/ --exclude node_modules,oauth

  Batasi ukuran:
    ./catter.sh app/ --max-size 200k

  Kombinasi:
    ./catter.sh app/ --ext go,html --exclude tmp,node_modules --max-size 500k

Output:
  File akan dibuat otomatis:
    catter_go.txt
    catter_html.txt
    dst.

Notes:
  - Extension dipisahkan dengan koma tanpa spasi
  - Exclude berlaku untuk semua subfolder
  - Aman untuk filename dengan spasi
  - File dibaca sekali (single-pass, efisien)

Tips:
  Cocok untuk:
    - audit codebase
    - feeding ke AI / LLM
    - backup isi file teks
    - analisis per bahasa (go, js, html, dll)

EOF
}

if [[ "$1" == "-h" || "$1" == "--help" || -z "$1" ]]; then
    show_help
    exit 0
fi

SOURCE_DIR="$1"
shift

EXTENSIONS=("go")
EXCLUDES=()
MAX_SIZE=""

while [[ "$#" -gt 0 ]]; do
    case "$1" in
        --ext)
            shift
            IFS=',' read -ra EXTENSIONS <<< "$1"
            ;;
        --exclude)
            shift
            IFS=',' read -ra EXCLUDES <<< "$1"
            ;;
        --max-size)
            shift
            MAX_SIZE="$1"
            ;;
        -h|--help)
            show_help
            exit 0
            ;;
        *)
            echo "Unknown parameter: $1"
            exit 1
            ;;
    esac
    shift
done

declare -A OUTPUT_FILES

for ext in "${EXTENSIONS[@]}"; do
    outfile="catter_${ext}.txt"
    > "$outfile"
    OUTPUT_FILES["$ext"]="$outfile"
done

FIND_EXPR=()
for ext in "${EXTENSIONS[@]}"; do
    FIND_EXPR+=( -name "*.${ext}" -o )
done
unset 'FIND_EXPR[${#FIND_EXPR[@]}-1]'

EXCLUDE_EXPR=()
for ex in "${EXCLUDES[@]}"; do
    EXCLUDE_EXPR+=( -not -path "*/${ex}/*" )
done

if [[ -n "$MAX_SIZE" ]]; then
    SIZE_EXPR=( -size "-$MAX_SIZE" )
else
    SIZE_EXPR=()
fi

find "$SOURCE_DIR" -type f \
    \( "${FIND_EXPR[@]}" \) \
    "${EXCLUDE_EXPR[@]}" \
    "${SIZE_EXPR[@]}" \
    -print0 |
while IFS= read -r -d '' file; do
    rel_path="${file#$SOURCE_DIR/}"

    ext="${file##*.}"

    if [[ -z "${OUTPUT_FILES[$ext]}" ]]; then
        continue
    fi

    outfile="${OUTPUT_FILES[$ext]}"

    echo "$rel_path" >> "$outfile"
    cat "$file" >> "$outfile"
    echo -e "\n\n" >> "$outfile"
done

echo "Selesai!"
for ext in "${EXTENSIONS[@]}"; do
    echo "- ${OUTPUT_FILES[$ext]}"
done