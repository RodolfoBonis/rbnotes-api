// Package audio guarda o áudio das notas no MinIO interno (S3-compatível). O upload vem em chunks
// pela API (cada PUT é uma parte) e é montado como um multipart upload do S3: o estado do upload
// (upload_id + ETags/tamanhos por parte) vive no Postgres entre as requisições. O objeto final fica
// em users/<uid>/notes/<noteId>.m4a. O download é por streaming, com suporte a Range.
//
// Concorrência: os PUTs de partes da MESMA nota são serializados por uma trava de linha
// (SELECT … FOR UPDATE em note_audio dentro de uma transação), evitando duas finalizações do mesmo
// multipart. O corpo da parte é transmitido direto para o S3 (sem carregar o chunk inteiro em
// memória), com Content-Length conhecido.
package audio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	dbgen "github.com/RodolfoBonis/rbnotes-api/db/gen"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/config"
)

const (
	defaultContentType = "audio/mp4"
	// minPartSize é o mínimo do S3 para partes NÃO finais de um multipart upload (5 MiB).
	minPartSize = 5 << 20
)

// Erros de domínio expostos à API.
var (
	// ErrNoUpload: parte > 1 recebida sem um upload iniciado (o cliente deve começar na parte 1).
	ErrNoUpload = errors.New("audio: upload não iniciado (envie a parte 1 primeiro)")
	// ErrNotFound: a nota não tem áudio montado.
	ErrNotFound = errors.New("audio: áudio não encontrado")
	// ErrBadPart: número de parte/total inválido.
	ErrBadPart = errors.New("audio: parte ou total inválido")
	// ErrTotalMismatch: o total informado difere do registrado no upload em andamento.
	ErrTotalMismatch = errors.New("audio: total de partes diferente do upload em andamento")
	// ErrPartTooSmall: parte não final menor que o mínimo do S3 (5 MiB).
	ErrPartTooSmall = fmt.Errorf("audio: partes não finais devem ter pelo menos %d bytes (5 MiB)", minPartSize)
	// ErrTooLarge: o áudio acumulado passou do teto AUDIO_MAX_BYTES.
	ErrTooLarge = errors.New("audio: áudio acima do tamanho máximo permitido")
	// ErrBadContentType: Content-Type fora da allowlist audio/*.
	ErrBadContentType = errors.New("audio: Content-Type inválido (esperado audio/*)")
	// ErrAlreadyCompleted: parte recebida depois de o upload já ter sido finalizado.
	ErrAlreadyCompleted = errors.New("audio: upload já finalizado")
	// ErrBadRange: header Range inválido para o objeto.
	ErrBadRange = errors.New("audio: range inválido")
)

// objectStore isola as operações do MinIO usadas pelo serviço (permite fake nos testes). *minio.Core
// satisfaz esta interface.
type objectStore interface {
	BucketExists(ctx context.Context, bucket string) (bool, error)
	MakeBucket(ctx context.Context, bucket string, opts minio.MakeBucketOptions) error
	NewMultipartUpload(ctx context.Context, bucket, object string, opts minio.PutObjectOptions) (string, error)
	PutObjectPart(ctx context.Context, bucket, object, uploadID string, partID int, data io.Reader, size int64, opts minio.PutObjectPartOptions) (minio.ObjectPart, error)
	CompleteMultipartUpload(ctx context.Context, bucket, object, uploadID string, parts []minio.CompletePart, opts minio.PutObjectOptions) (minio.UploadInfo, error)
	AbortMultipartUpload(ctx context.Context, bucket, object, uploadID string) error
	GetObject(ctx context.Context, bucket, object string, opts minio.GetObjectOptions) (io.ReadCloser, minio.ObjectInfo, http.Header, error)
	RemoveObject(ctx context.Context, bucket, object string, opts minio.RemoveObjectOptions) error
}

// Service orquestra o multipart upload e o download.
type Service struct {
	store    objectStore
	bucket   string
	q        *dbgen.Queries
	pool     *pgxpool.Pool
	maxBytes int64
	logger   *slog.Logger
}

// New constrói o serviço de áudio a partir da config do MinIO.
func New(ctx context.Context, cfg config.MinIO, maxBytes int64, q *dbgen.Queries, pool *pgxpool.Pool, logger *slog.Logger) (*Service, error) {
	endpoint, secure := cfg.Endpoint()
	core, err := minio.NewCore(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessID, cfg.SecretKey, ""),
		Secure: secure,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("audio: cliente minio: %w", err)
	}
	s := &Service{store: core, bucket: cfg.Bucket, q: q, pool: pool, maxBytes: maxBytes, logger: logger}
	if err := s.ensureBucket(ctx, cfg.Region); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Service) ensureBucket(ctx context.Context, region string) error {
	ok, err := s.store.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("audio: checar bucket: %w", err)
	}
	if ok {
		return nil
	}
	if err := s.store.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{Region: region}); err != nil {
		return fmt.Errorf("audio: criar bucket: %w", err)
	}
	return nil
}

// objectKey monta a chave do objeto no bucket (escopo por usuário).
func objectKey(userID, noteID string) string {
	return "users/" + userID + "/notes/" + noteID + ".m4a"
}

// normalizeContentType aplica a allowlist audio/*. Vazio ou octet-stream (o desktop manda os chunks
// crus) vira o default audio/mp4; audio/* é mantido; qualquer outro é rejeitado.
func normalizeContentType(ct string) (string, error) {
	ct = strings.TrimSpace(strings.ToLower(ct))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if ct == "" || ct == "application/octet-stream" {
		return defaultContentType, nil
	}
	if strings.HasPrefix(ct, "audio/") {
		return ct, nil
	}
	return "", ErrBadContentType
}

// PutPart recebe uma parte do áudio. part começa em 1; total é o número de partes. Quando a última
// parte chega (todas recebidas), o multipart é concluído e devolve completed=true. size é o tamanho
// do chunk em bytes (Content-Length) e o corpo é transmitido direto para o S3. Serializa os PUTs da
// mesma nota via trava de linha.
func (s *Service) PutPart(ctx context.Context, userID pgtype.UUID, noteID string, part, total int, contentType string, body io.Reader, size int64) (bool, error) {
	if part < 1 || total < 1 || part > total {
		return false, ErrBadPart
	}
	ct, err := normalizeContentType(contentType)
	if err != nil {
		return false, err
	}
	// Partes não finais precisam respeitar o mínimo do S3 (a última pode ser menor).
	if part < total && size < minPartSize {
		return false, ErrPartTooSmall
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("audio: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	uid := uuidString(userID)
	key := objectKey(uid, noteID)

	row, err := qtx.GetAudioForUpdate(ctx, dbgen.GetAudioForUpdateParams{UserID: userID, NoteID: noteID})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if part != 1 {
			return false, ErrNoUpload
		}
		row, err = s.initUpload(ctx, qtx, userID, noteID, key, total, ct)
		if err != nil {
			return false, err
		}
	case err != nil:
		return false, fmt.Errorf("audio: carregar estado: %w", err)
	case part == 1 && (row.UploadID == "" || row.Completed):
		// Reinício do upload (nova gravação da mesma nota): abandona o anterior e recomeça.
		s.abort(ctx, key, row.UploadID)
		row, err = s.initUpload(ctx, qtx, userID, noteID, key, total, ct)
		if err != nil {
			return false, err
		}
	case row.Completed:
		// Upload já finalizado: reenviar a última parte é sucesso idempotente; qualquer outra parte
		// (> 1) depois do fim é conflito.
		if part == total {
			return true, tx.Commit(ctx)
		}
		return false, ErrAlreadyCompleted
	case row.UploadID == "":
		return false, ErrNoUpload
	default:
		if int(row.TotalParts) != total {
			return false, ErrTotalMismatch
		}
		if part > int(row.TotalParts) {
			return false, ErrBadPart
		}
	}

	// Teto de bytes: calcula o total prospectivo (substituindo o tamanho desta parte), ANTES de
	// enviar, para não desperdiçar o upload.
	if s.maxBytes > 0 {
		sizes, derr := decodeSizes(row.PartSizes)
		if derr != nil {
			return false, derr
		}
		sizes[part] = size
		var sum int64
		for _, v := range sizes {
			sum += v
		}
		if sum > s.maxBytes {
			s.abort(ctx, key, row.UploadID)
			return false, ErrTooLarge
		}
	}

	objPart, err := s.store.PutObjectPart(ctx, s.bucket, key, row.UploadID, part, body, size, minio.PutObjectPartOptions{})
	if err != nil {
		if isNoSuchUpload(err) {
			// O multipart sumiu no MinIO (expirou/abortado): recomeça do zero. O corpo já foi
			// consumido, então o cliente precisa reenviar a partir da parte 1.
			if _, rerr := s.initUpload(ctx, qtx, userID, noteID, key, total, ct); rerr != nil {
				return false, rerr
			}
			if cerr := tx.Commit(ctx); cerr != nil {
				return false, fmt.Errorf("audio: commit reinit: %w", cerr)
			}
			return false, ErrNoUpload
		}
		return false, fmt.Errorf("audio: enviar parte: %w", err)
	}

	etags, err := mergePart(row.Parts, part, objPart.ETag)
	if err != nil {
		return false, err
	}
	partSize, err := sizeJSON(part, size)
	if err != nil {
		return false, err
	}
	updated, err := qtx.SetAudioPart(ctx, dbgen.SetAudioPartParams{UserID: userID, NoteID: noteID, Part: etags, PartSize: partSize})
	if err != nil {
		return false, fmt.Errorf("audio: gravar parte: %w", err)
	}

	parts, err := decodeParts(updated.Parts)
	if err != nil {
		return false, err
	}
	if len(parts) < int(updated.TotalParts) {
		return false, tx.Commit(ctx)
	}
	if err := s.complete(ctx, qtx, userID, noteID, key, updated.UploadID, parts); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (s *Service) initUpload(ctx context.Context, qtx *dbgen.Queries, userID pgtype.UUID, noteID, key string, total int, contentType string) (dbgen.NoteAudio, error) {
	uploadID, err := s.store.NewMultipartUpload(ctx, s.bucket, key, minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return dbgen.NoteAudio{}, fmt.Errorf("audio: iniciar multipart: %w", err)
	}
	//nolint:gosec // total vem validado (>=1, <= part); cabe em int32.
	row, err := qtx.UpsertAudioInit(ctx, dbgen.UpsertAudioInitParams{
		UserID: userID, NoteID: noteID, ObjectKey: key, UploadID: uploadID,
		TotalParts: int32(total), ContentType: contentType,
	})
	if err != nil {
		s.abort(ctx, key, uploadID)
		return dbgen.NoteAudio{}, fmt.Errorf("audio: registrar upload: %w", err)
	}
	return row, nil
}

// complete finaliza o multipart (partes em ordem) e grava o tamanho final.
func (s *Service) complete(ctx context.Context, qtx *dbgen.Queries, userID pgtype.UUID, noteID, key, uploadID string, parts map[int]string) error {
	nums := make([]int, 0, len(parts))
	for n := range parts {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	complete := make([]minio.CompletePart, 0, len(nums))
	for _, n := range nums {
		complete = append(complete, minio.CompletePart{PartNumber: n, ETag: parts[n]})
	}
	info, err := s.store.CompleteMultipartUpload(ctx, s.bucket, key, uploadID, complete, minio.PutObjectOptions{})
	if err != nil {
		return fmt.Errorf("audio: concluir multipart: %w", err)
	}
	if err := qtx.CompleteAudio(ctx, dbgen.CompleteAudioParams{UserID: userID, NoteID: noteID, Size: info.Size}); err != nil {
		return fmt.Errorf("audio: marcar concluído: %w", err)
	}
	return nil
}

// abort descarta um multipart pendente (best effort; só loga em erro).
func (s *Service) abort(ctx context.Context, key, uploadID string) {
	if uploadID == "" {
		return
	}
	if err := s.store.AbortMultipartUpload(ctx, s.bucket, key, uploadID); err != nil {
		s.logger.Warn("audio: abortar multipart falhou", slog.String("err", err.Error()))
	}
}

// Delete apaga o áudio de uma nota (escopo por usuário) e é IDEMPOTENTE: se não há nada registrado
// nem objeto, ainda assim retorna sucesso. Aborta o multipart em andamento (se houver) e remove o
// objeto do bucket. Usado por DELETE /notes/{id}/audio (apagar a nota / "apagar todos os dados").
func (s *Service) Delete(ctx context.Context, userID pgtype.UUID, noteID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("audio: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	key := objectKey(uuidString(userID), noteID)
	row, err := qtx.GetAudioForUpdate(ctx, dbgen.GetAudioForUpdateParams{UserID: userID, NoteID: noteID})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Nada registrado: remove um objeto órfão por garantia (idempotente) e confirma.
		s.removeObject(ctx, key)
		return tx.Commit(ctx)
	case err != nil:
		return fmt.Errorf("audio: carregar estado: %w", err)
	}
	// Multipart ainda aberto: aborta para não deixar partes penduradas no storage.
	if row.UploadID != "" && !row.Completed {
		s.abort(ctx, row.ObjectKey, row.UploadID)
	}
	s.removeObject(ctx, row.ObjectKey)
	if err := qtx.DeleteAudio(ctx, dbgen.DeleteAudioParams{UserID: userID, NoteID: noteID}); err != nil {
		return fmt.Errorf("audio: apagar estado: %w", err)
	}
	return tx.Commit(ctx)
}

// removeObject remove o objeto do bucket (best effort; "não existe" é sucesso — idempotência).
func (s *Service) removeObject(ctx context.Context, key string) {
	if err := s.store.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil && !isNoSuchKey(err) {
		s.logger.Warn("audio: remover objeto falhou", slog.String("err", err.Error()))
	}
}

// Content descreve o áudio a servir (completo ou um intervalo Range).
type Content struct {
	Body        io.ReadCloser
	ContentType string
	Size        int64 // tamanho total do objeto
	Partial     bool  // true quando é uma resposta parcial (Range)
	Start, End  int64 // intervalo inclusivo (quando Partial)
}

// Length devolve o número de bytes a enviar (do intervalo quando parcial; o objeto todo caso
// contrário).
func (c Content) Length() int64 {
	if c.Partial {
		return c.End - c.Start + 1
	}
	return c.Size
}

// Open abre o áudio da nota para download. rangeHeader vazio devolve o objeto inteiro; um Range
// válido ("bytes=i-j") devolve o intervalo (Content.Partial=true). O chamador deve fechar o Body.
func (s *Service) Open(ctx context.Context, userID pgtype.UUID, noteID, rangeHeader string) (*Content, error) {
	row, err := s.q.GetAudio(ctx, dbgen.GetAudioParams{UserID: userID, NoteID: noteID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !row.Completed) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("audio: carregar estado: %w", err)
	}
	ct, cterr := normalizeContentType(row.ContentType)
	if cterr != nil {
		ct = defaultContentType // defensivo: sempre servimos um tipo válido.
	}
	total := row.Size

	opts := minio.GetObjectOptions{}
	start, end, partial, rerr := parseRange(rangeHeader, total)
	if rerr != nil {
		return nil, rerr
	}
	if partial {
		if serr := opts.SetRange(start, end); serr != nil {
			return nil, ErrBadRange
		}
	}
	obj, _, _, err := s.store.GetObject(ctx, s.bucket, row.ObjectKey, opts)
	if err != nil {
		return nil, fmt.Errorf("audio: obter objeto: %w", err)
	}
	return &Content{Body: obj, ContentType: ct, Size: total, Partial: partial, Start: start, End: end}, nil
}

// parseRange interpreta um header Range simples ("bytes=start-end", "bytes=start-", "bytes=-suffix")
// contra o tamanho total. Vazio = objeto inteiro. Devolve ErrBadRange se não satisfazível.
func parseRange(h string, total int64) (start, end int64, partial bool, err error) {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0, 0, false, nil
	}
	if total <= 0 || !strings.HasPrefix(h, "bytes=") {
		return 0, 0, false, ErrBadRange
	}
	spec := strings.TrimPrefix(h, "bytes=")
	if strings.Contains(spec, ",") {
		return 0, 0, false, ErrBadRange // múltiplos intervalos não suportados
	}
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, 0, false, ErrBadRange
	}
	startStr, endStr := spec[:dash], spec[dash+1:]
	switch startStr {
	case "": // sufixo: últimos N bytes
		n, perr := strconv.ParseInt(endStr, 10, 64)
		if perr != nil || n <= 0 {
			return 0, 0, false, ErrBadRange
		}
		if n > total {
			n = total
		}
		return total - n, total - 1, true, nil
	default:
		start, perr := strconv.ParseInt(startStr, 10, 64)
		if perr != nil || start < 0 || start >= total {
			return 0, 0, false, ErrBadRange
		}
		end := total - 1
		if endStr != "" {
			e, eerr := strconv.ParseInt(endStr, 10, 64)
			if eerr != nil || e < start {
				return 0, 0, false, ErrBadRange
			}
			end = e
		}
		if end > total-1 {
			end = total - 1
		}
		return start, end, true, nil
	}
}

func isNoSuchUpload(err error) bool {
	return minio.ToErrorResponse(err).Code == "NoSuchUpload"
}

func isNoSuchKey(err error) bool {
	return minio.ToErrorResponse(err).Code == "NoSuchKey"
}

// mergePart devolve o mapa de ETags (jsonb) com a nova parte adicionada.
func mergePart(current []byte, part int, etag string) ([]byte, error) {
	m, err := decodeParts(current)
	if err != nil {
		return nil, err
	}
	m[part] = etag
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[strconv.Itoa(k)] = v
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("audio: serializar partes: %w", err)
	}
	return b, nil
}

// decodeParts lê o mapa {"1":"etag"} em map[int]string.
func decodeParts(raw []byte) (map[int]string, error) {
	out := map[int]string{}
	if len(raw) == 0 {
		return out, nil
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("audio: ler partes: %w", err)
	}
	for k, v := range m {
		n, err := strconv.Atoi(k)
		if err != nil {
			continue
		}
		out[n] = v
	}
	return out, nil
}

// sizeJSON monta o jsonb {"<part>": <size>} para o merge em part_sizes.
func sizeJSON(part int, size int64) ([]byte, error) {
	b, err := json.Marshal(map[string]int64{strconv.Itoa(part): size})
	if err != nil {
		return nil, fmt.Errorf("audio: serializar tamanho: %w", err)
	}
	return b, nil
}

// decodeSizes lê o mapa {"1":1234} em map[int]int64.
func decodeSizes(raw []byte) (map[int]int64, error) {
	out := map[int]int64{}
	if len(raw) == 0 {
		return out, nil
	}
	var m map[string]int64
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("audio: ler tamanhos: %w", err)
	}
	for k, v := range m {
		n, err := strconv.Atoi(k)
		if err != nil {
			continue
		}
		out[n] = v
	}
	return out, nil
}

func uuidString(u pgtype.UUID) string {
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
