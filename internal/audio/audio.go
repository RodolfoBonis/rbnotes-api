// Package audio guarda o áudio das notas no MinIO interno (S3-compatível). O upload vem em chunks
// pela API (cada PUT é uma parte) e é montado como um multipart upload do S3: o estado do upload
// (upload_id + ETags por parte) vive no Postgres entre as requisições. O objeto final fica em
// users/<uid>/notes/<noteId>.m4a. O download é por streaming.
package audio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	dbgen "github.com/RodolfoBonis/rbnotes-api/db/gen"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/config"
)

const defaultContentType = "audio/mp4"

// Erros de domínio expostos à API.
var (
	// ErrNoUpload: parte > 1 recebida sem um upload iniciado (o cliente deve começar na parte 1).
	ErrNoUpload = errors.New("audio: upload não iniciado (envie a parte 1 primeiro)")
	// ErrNotFound: a nota não tem áudio montado.
	ErrNotFound = errors.New("audio: áudio não encontrado")
	// ErrBadPart: número de parte/total inválido.
	ErrBadPart = errors.New("audio: parte ou total inválido")
)

// Service orquestra o multipart upload e o download.
type Service struct {
	core   *minio.Core
	bucket string
	q      *dbgen.Queries
	logger *slog.Logger
}

// New constrói o serviço de áudio a partir da config do MinIO.
func New(ctx context.Context, cfg config.MinIO, q *dbgen.Queries, logger *slog.Logger) (*Service, error) {
	core, err := minio.NewCore(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("audio: cliente minio: %w", err)
	}
	s := &Service{core: core, bucket: cfg.Bucket, q: q, logger: logger}
	if err := s.ensureBucket(ctx, cfg.Region); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Service) ensureBucket(ctx context.Context, region string) error {
	ok, err := s.core.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("audio: checar bucket: %w", err)
	}
	if ok {
		return nil
	}
	if err := s.core.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{Region: region}); err != nil {
		return fmt.Errorf("audio: criar bucket: %w", err)
	}
	return nil
}

// objectKey monta a chave do objeto no bucket (escopo por usuário).
func objectKey(userID, noteID string) string {
	return "users/" + userID + "/notes/" + noteID + ".m4a"
}

// PutPart recebe uma parte do áudio. part começa em 1; total é o número de partes. Quando a última
// parte chega (todas as partes recebidas), o multipart é concluído. Devolve completed=true nesse
// caso. size é o tamanho do chunk em bytes.
func (s *Service) PutPart(ctx context.Context, userID pgtype.UUID, noteID string, part, total int, contentType string, body io.Reader, size int64) (bool, error) {
	if part < 1 || total < 1 || part > total {
		return false, ErrBadPart
	}
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = defaultContentType
	}
	uid := uuidString(userID)
	key := objectKey(uid, noteID)

	row, err := s.q.GetAudio(ctx, dbgen.GetAudioParams{UserID: userID, NoteID: noteID})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if part != 1 {
			return false, ErrNoUpload
		}
		row, err = s.initUpload(ctx, userID, noteID, key, total, contentType)
		if err != nil {
			return false, err
		}
	case err != nil:
		return false, fmt.Errorf("audio: carregar estado: %w", err)
	case part == 1 && (row.UploadID == "" || row.Completed):
		// Reinício do upload (nova gravação da mesma nota): abandona o anterior e recomeça.
		s.abort(ctx, key, row.UploadID)
		row, err = s.initUpload(ctx, userID, noteID, key, total, contentType)
		if err != nil {
			return false, err
		}
	case row.UploadID == "":
		return false, ErrNoUpload
	}

	objPart, err := s.core.PutObjectPart(ctx, s.bucket, key, row.UploadID, part, body, size, minio.PutObjectPartOptions{})
	if err != nil {
		return false, fmt.Errorf("audio: enviar parte: %w", err)
	}

	etags, err := mergePart(row.Parts, part, objPart.ETag)
	if err != nil {
		return false, err
	}
	updated, err := s.q.SetAudioPart(ctx, dbgen.SetAudioPartParams{UserID: userID, NoteID: noteID, Part: etags})
	if err != nil {
		return false, fmt.Errorf("audio: gravar parte: %w", err)
	}

	parts, err := decodeParts(updated.Parts)
	if err != nil {
		return false, err
	}
	if len(parts) < int(updated.TotalParts) {
		return false, nil
	}
	return true, s.complete(ctx, userID, noteID, key, updated.UploadID, parts)
}

func (s *Service) initUpload(ctx context.Context, userID pgtype.UUID, noteID, key string, total int, contentType string) (dbgen.NoteAudio, error) {
	uploadID, err := s.core.NewMultipartUpload(ctx, s.bucket, key, minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return dbgen.NoteAudio{}, fmt.Errorf("audio: iniciar multipart: %w", err)
	}
	//nolint:gosec // total vem validado (>=1, <= part); cabe em int32.
	row, err := s.q.UpsertAudioInit(ctx, dbgen.UpsertAudioInitParams{
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
func (s *Service) complete(ctx context.Context, userID pgtype.UUID, noteID, key, uploadID string, parts map[int]string) error {
	nums := make([]int, 0, len(parts))
	for n := range parts {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	complete := make([]minio.CompletePart, 0, len(nums))
	for _, n := range nums {
		complete = append(complete, minio.CompletePart{PartNumber: n, ETag: parts[n]})
	}
	info, err := s.core.CompleteMultipartUpload(ctx, s.bucket, key, uploadID, complete, minio.PutObjectOptions{})
	if err != nil {
		return fmt.Errorf("audio: concluir multipart: %w", err)
	}
	if err := s.q.CompleteAudio(ctx, dbgen.CompleteAudioParams{UserID: userID, NoteID: noteID, Size: info.Size}); err != nil {
		return fmt.Errorf("audio: marcar concluído: %w", err)
	}
	return nil
}

// abort descarta um multipart pendente (best effort; só loga em erro).
func (s *Service) abort(ctx context.Context, key, uploadID string) {
	if uploadID == "" {
		return
	}
	if err := s.core.AbortMultipartUpload(ctx, s.bucket, key, uploadID); err != nil {
		s.logger.Warn("audio: abortar multipart falhou", slog.String("err", err.Error()))
	}
}

// Download devolve um leitor do áudio da nota (o chamador deve fechar). info traz tamanho e tipo.
func (s *Service) Download(ctx context.Context, userID pgtype.UUID, noteID string) (io.ReadCloser, minio.ObjectInfo, error) {
	row, err := s.q.GetAudio(ctx, dbgen.GetAudioParams{UserID: userID, NoteID: noteID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !row.Completed) {
		return nil, minio.ObjectInfo{}, ErrNotFound
	}
	if err != nil {
		return nil, minio.ObjectInfo{}, fmt.Errorf("audio: carregar estado: %w", err)
	}
	obj, info, _, err := s.core.GetObject(ctx, s.bucket, row.ObjectKey, minio.GetObjectOptions{})
	if err != nil {
		return nil, minio.ObjectInfo{}, fmt.Errorf("audio: obter objeto: %w", err)
	}
	return obj, info, nil
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

func uuidString(u pgtype.UUID) string {
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
