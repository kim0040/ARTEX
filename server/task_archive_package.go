package server

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	pgdb "github.com/Autumn-27/artex/db"
	"github.com/klauspost/compress/zstd"
)

const (
	archiveDirMode  = 0o700
	archiveFileMode = 0o600
	maxArchiveFiles = 1_000_000
	maxArchiveBytes = int64(1 << 47) // 깨진 헤더를 위한 128 TiB 안전 상한.
)

type archiveFileMove struct {
	Source   string `json:"source"`
	Relative string `json:"relative"`
}

type archiveStageJournal struct {
	ArchiveID int64             `json:"archive_id"`
	TaskID    string            `json:"task_id"`
	Moves     []archiveFileMove `json:"moves"`
}

type archiveRestoreJournal struct {
	ArchiveID int64                 `json:"archive_id"`
	TaskID    int64                 `json:"task_id"`
	Moves     []restoredArchivePath `json:"moves"`
}

type taskArchiveFileStage struct {
	root    string
	payload string
	journal archiveStageJournal
	done    bool
}

type restoredArchivePath struct {
	Source      string
	Destination string
}

type taskArchiveRestoreFiles struct {
	extracted string
	moves     []restoredArchivePath
	done      bool
}

func writeArchiveJournal(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, archiveFileMode)
	if err != nil {
		return err
	}
	if _, err := file.Write(raw); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func taskArchiveRoot(dataDir string) string {
	return filepath.Join(dataDir, "archives", "tasks")
}

func taskArchivePath(dataDir string, archiveID int64, taskID string) string {
	return filepath.Join(taskArchiveRoot(dataDir), fmt.Sprintf("task-%s-%d.tar.zst", taskID, archiveID))
}

func stageTaskArchiveFiles(dataDir string, archiveID int64, taskID string, explorationID int64) (*taskArchiveFileStage, error) {
	root := filepath.Join(taskArchiveRoot(dataDir), ".staging", strconv.FormatInt(archiveID, 10))
	stage := &taskArchiveFileStage{root: root, payload: filepath.Join(root, "payload")}
	journalPath := filepath.Join(root, "journal.json")
	if raw, err := os.ReadFile(journalPath); err == nil {
		if err := json.Unmarshal(raw, &stage.journal); err != nil {
			return nil, fmt.Errorf("read task archive staging journal: %w", err)
		}
		// 저널은 첫 이름 바꾸기 전에 씁니다. 그래서 저널이 있다고 적재가
		// 끝난 것은 아닙니다. 이전 라운드가 루프 중간에 실패하고, 되돌리기까지
		// 오류가 나면 루트는 남습니다. 옮기기를 다시 하고,
		// 대화 기록이나 작업 파일이 빠진 적재를 패키지로 만들지 않습니다.
		if err := os.MkdirAll(filepath.Join(stage.payload, "files"), archiveDirMode); err != nil {
			return nil, err
		}
		if err := stage.applyMoves(); err != nil {
			_ = stage.rollback()
			return nil, err
		}
		return stage, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(stage.payload, "files"), archiveDirMode); err != nil {
		return nil, err
	}
	stage.journal = archiveStageJournal{ArchiveID: archiveID, TaskID: taskID}
	targets := []archiveFileMove{}
	taskDir := filepath.Join(dataDir, "tasks", taskID)
	if _, err := os.Lstat(taskDir); err == nil {
		targets = append(targets, archiveFileMove{Source: taskDir, Relative: filepath.Join("files", "tasks", taskID)})
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	transcriptDir := filepath.Join(dataDir, "transcripts")
	entries, err := os.ReadDir(transcriptDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	prefix := fmt.Sprintf("exp%d-", explorationID)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || (!entry.IsDir() && !strings.HasSuffix(name, ".jsonl")) {
			continue
		}
		targets = append(targets, archiveFileMove{
			Source: filepath.Join(transcriptDir, name), Relative: filepath.Join("files", "transcripts", name),
		})
	}
	// 첫 이름 바꾸기 전에 완성된 계획을 저장합니다. 그래서 복구는
	// 프로세스가 갑자기 죽어도, 옮긴 앞부분만 되돌릴 수 있습니다.
	stage.journal.Moves = targets
	if err := writeArchiveJournal(journalPath, stage.journal); err != nil {
		_ = stage.rollback()
		return nil, err
	}
	if err := stage.applyMoves(); err != nil {
		_ = stage.rollback()
		return nil, err
	}
	return stage, nil
}

// applyMoves는 저널에 적힌 이름 바꾸기를 수행합니다. 이미 적재 안에
// 있는 항목은 건너뜁니다. 끊긴 준비 라운드를 그 자리에서
// 이어 가고, 같은 것을 두 번 옮기지 않습니다.
func (s *taskArchiveFileStage) applyMoves() error {
	for _, move := range s.journal.Moves {
		destination := filepath.Join(s.payload, move.Relative)
		if _, err := os.Lstat(destination); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		if _, err := os.Lstat(move.Source); err != nil {
			if os.IsNotExist(err) {
				// 양쪽 다 없습니다. 저널을 쓴 뒤에 보관 흐름 밖에서
				// 경로가 지워진 것입니다. 준비할 것이 없습니다.
				continue
			}
			return err
		}
		if err := os.MkdirAll(filepath.Dir(destination), archiveDirMode); err != nil {
			return err
		}
		if err := os.Rename(move.Source, destination); err != nil {
			return fmt.Errorf("stage task archive path %s: %w", move.Source, err)
		}
	}
	return nil
}

func (s *taskArchiveFileStage) rollback() error {
	if s == nil || s.done {
		return nil
	}
	var errs []error
	for i := len(s.journal.Moves) - 1; i >= 0; i-- {
		move := s.journal.Moves[i]
		staged := filepath.Join(s.payload, move.Relative)
		if _, err := os.Lstat(staged); os.IsNotExist(err) {
			continue
		} else if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := os.Lstat(move.Source); err == nil {
			errs = append(errs, fmt.Errorf("archive rollback destination exists: %s", move.Source))
			continue
		} else if !os.IsNotExist(err) {
			errs = append(errs, err)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(move.Source), 0o755); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.Rename(staged, move.Source); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		if err := os.RemoveAll(s.root); err != nil {
			errs = append(errs, err)
		}
	}
	s.done = true
	return errors.Join(errs...)
}

func (s *taskArchiveFileStage) commit() error {
	if s == nil || s.done {
		return nil
	}
	s.done = true
	return os.RemoveAll(s.root)
}

func installTaskArchiveFiles(dataDir, extractedDir, taskID string, archiveID int64) (*taskArchiveRestoreFiles, error) {
	stage := &taskArchiveRestoreFiles{extracted: extractedDir}
	numericTaskID, err := strconv.ParseInt(taskID, 10, 64)
	if err != nil || numericTaskID <= 0 {
		return nil, fmt.Errorf("invalid restore task id %q", taskID)
	}
	sources := []restoredArchivePath{}
	workspace := filepath.Join(extractedDir, "files", "tasks", taskID)
	if _, err := os.Lstat(workspace); err == nil {
		sources = append(sources, restoredArchivePath{Source: workspace, Destination: filepath.Join(dataDir, "tasks", taskID)})
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	transcripts := filepath.Join(extractedDir, "files", "transcripts")
	entries, err := os.ReadDir(transcripts)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, entry := range entries {
		sources = append(sources, restoredArchivePath{
			Source: filepath.Join(transcripts, entry.Name()), Destination: filepath.Join(dataDir, "transcripts", entry.Name()),
		})
	}
	for _, move := range sources {
		if _, err := os.Lstat(move.Destination); err == nil {
			return nil, fmt.Errorf("restore destination already exists: %s", move.Destination)
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	stage.moves = sources
	journal := archiveRestoreJournal{ArchiveID: archiveID, TaskID: numericTaskID, Moves: sources}
	if err := writeArchiveJournal(filepath.Join(extractedDir, "restore-journal.json"), journal); err != nil {
		return nil, err
	}
	for _, move := range sources {
		if err := os.MkdirAll(filepath.Dir(move.Destination), 0o755); err != nil {
			_ = stage.rollback()
			return nil, err
		}
		if err := os.Rename(move.Source, move.Destination); err != nil {
			_ = stage.rollback()
			return nil, err
		}
	}
	return stage, nil
}

func (s *taskArchiveRestoreFiles) rollback() error {
	if s == nil || s.done {
		return nil
	}
	var errs []error
	for i := len(s.moves) - 1; i >= 0; i-- {
		move := s.moves[i]
		if _, err := os.Lstat(move.Destination); os.IsNotExist(err) {
			continue
		} else if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := os.Lstat(move.Source); err == nil {
			errs = append(errs, fmt.Errorf("restore rollback source exists: %s", move.Source))
			continue
		} else if !os.IsNotExist(err) {
			errs = append(errs, err)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(move.Source), archiveDirMode); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.Rename(move.Destination, move.Source); err != nil {
			errs = append(errs, err)
		}
	}
	s.done = true
	return errors.Join(errs...)
}

func (s *taskArchiveRestoreFiles) commit() error {
	if s == nil || s.done {
		return nil
	}
	s.done = true
	return os.RemoveAll(s.extracted)
}

// recoverTaskArchiveRestoreStages는 끊긴 복원이 남긴 파일 설치를 정리합니다.
// PostgreSQL이 확정했으면 설치된 파일이 기준이고,
// 풀어 둔 디렉터리만 낡았습니다. 아니면 끝난 이름 바꾸기를 모두
// 되돌려, 영구 복원 작업이 깨끗한 대상에서 다시 시도하게 합니다.
func recoverTaskArchiveRestoreStages(dataDir string, pg *pgdb.DB) error {
	parent := filepath.Join(taskArchiveRoot(dataDir), ".restore")
	entries, err := os.ReadDir(parent)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		root := filepath.Join(parent, entry.Name())
		raw, err := os.ReadFile(filepath.Join(root, "restore-journal.json"))
		if os.IsNotExist(err) {
			// 대상 이름 바꾸기 전에 풀기가 끊겼습니다.
			if err := os.RemoveAll(root); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var journal archiveRestoreJournal
		if err := json.Unmarshal(raw, &journal); err != nil {
			errs = append(errs, err)
			continue
		}
		restored, err := pg.IsTaskArchiveRestored(journal.ArchiveID)
		if errors.Is(err, pgdb.ErrTaskArchiveNotFound) && journal.TaskID > 0 {
			task, taskErr := pg.GetTask(journal.TaskID)
			if taskErr != nil {
				errs = append(errs, taskErr)
				continue
			}
			restored = task != nil
			err = nil
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if restored {
			if err := os.RemoveAll(root); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		stage := &taskArchiveRestoreFiles{extracted: root, moves: journal.Moves}
		if err := stage.rollback(); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.RemoveAll(root); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// recoverTaskArchiveStages는 끊긴 보관이 남긴 파일 이름 바꾸기를 정리합니다.
// 확정된 차가운 작업은 검증된 패키지가 있으므로, 낡은 준비는
// 버릴 수 있습니다. 아니면 영구 작업이 다시 시도하기 전에 파일을 되돌립니다.
// 초보용: 엔진이 작업을 보관하다 끊기면, 파일을 패키지와 맞게 되돌리거나 버립니다.
func recoverTaskArchiveStages(dataDir string, pg *pgdb.DB) error {
	parent := filepath.Join(taskArchiveRoot(dataDir), ".staging")
	entries, err := os.ReadDir(parent)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		root := filepath.Join(parent, entry.Name())
		raw, err := os.ReadFile(filepath.Join(root, "journal.json"))
		if os.IsNotExist(err) {
			// 준비는 디렉터리 트리를 만든 뒤 저널을 쓰기 전에 죽었거나
			// (이름 바꾸기는 아직 없어 적재는 비어 있음), 확정/되돌리기가
			// 루트를 지우다 중간에 실패했습니다. 그때 적재에는
			// 이미 패키지 안에 있는 사본만 있습니다. 저널이 없으면 되돌릴 것이 없고,
			// 여기서 오류를 보고하면 보관 워커가 영영 시작을 못 하므로
			// 디렉터리를 버립니다.
			if err := os.RemoveAll(root); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var journal archiveStageJournal
		if err := json.Unmarshal(raw, &journal); err != nil {
			errs = append(errs, err)
			continue
		}
		archive, getErr := pg.GetTaskArchive(journal.ArchiveID)
		if getErr != nil {
			errs = append(errs, getErr)
			continue
		}
		if archive != nil && archive.State == pgdb.ArchiveReady {
			if err := os.RemoveAll(root); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		stage := &taskArchiveFileStage{root: root, payload: filepath.Join(root, "payload"), journal: journal}
		if err := stage.rollback(); err != nil {
			errs = append(errs, err)
		}
		if archive != nil && archive.ArchivePath != "" {
			_ = os.Remove(archive.ArchivePath)
		}
	}
	return errors.Join(errs...)
}

func recoverTaskArchiveDeletePackages(dataDir string, pg *pgdb.DB) error {
	root := taskArchiveRoot(dataDir)
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		marker := strings.LastIndex(name, ".deleting-")
		if marker < 0 {
			continue
		}
		archiveID, err := strconv.ParseInt(name[marker+len(".deleting-"):], 10, 64)
		if err != nil || archiveID <= 0 {
			continue
		}
		staged := filepath.Join(root, name)
		original := filepath.Join(root, name[:marker])
		archive, err := pg.GetTaskArchive(archiveID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if archive == nil {
			if err := os.Remove(staged); err != nil && !os.IsNotExist(err) {
				errs = append(errs, err)
			}
			continue
		}
		if archive.State == pgdb.DeleteQueued || archive.State == pgdb.Deleting || archive.State == pgdb.DeleteFailed {
			// 여러 번 해도 되는 삭제 워커가 준비된 경로를 직접 소비합니다.
			continue
		}
		if _, err := os.Lstat(original); err == nil {
			errs = append(errs, fmt.Errorf("archive delete recovery destination exists: %s", original))
			continue
		} else if !os.IsNotExist(err) {
			errs = append(errs, err)
			continue
		}
		if err := os.Rename(staged, original); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func stageTaskArchivePackageDelete(archivePath string, archiveID int64) (string, bool, error) {
	staged := archivePath + fmt.Sprintf(".deleting-%d", archiveID)
	if _, err := os.Lstat(staged); err == nil {
		if _, originalErr := os.Lstat(archivePath); originalErr == nil {
			return staged, false, errors.New("아카이브 패키지 원본 파일과 삭제 임시 파일이 동시에 존재합니다")
		} else if !os.IsNotExist(originalErr) {
			return staged, false, originalErr
		}
		return staged, true, nil
	} else if !os.IsNotExist(err) {
		return staged, false, err
	}
	if err := os.Rename(archivePath, staged); err == nil {
		return staged, true, nil
	} else if !os.IsNotExist(err) {
		return staged, false, err
	}
	return staged, false, nil
}

func writeTaskArchivePackage(path, payloadDir string, snapshot *pgdb.TaskArchiveSnapshot) (originalSize, compressedSize int64, checksum string, err error) {
	if snapshot == nil {
		return 0, 0, "", errors.New("nil task archive snapshot")
	}
	if err = os.MkdirAll(filepath.Dir(path), archiveDirMode); err != nil {
		return 0, 0, "", err
	}
	manifestFile, err := os.OpenFile(
		filepath.Join(payloadDir, "manifest.json"),
		os.O_CREATE|os.O_TRUNC|os.O_WRONLY,
		archiveFileMode,
	)
	if err != nil {
		return 0, 0, "", err
	}
	encodeErr := json.NewEncoder(manifestFile).Encode(snapshot)
	var syncErr error
	if encodeErr == nil {
		syncErr = manifestFile.Sync()
	}
	if err := errors.Join(encodeErr, syncErr, manifestFile.Close()); err != nil {
		return 0, 0, "", err
	}
	temporary := path + ".partial"
	_ = os.Remove(temporary)
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, archiveFileMode)
	if err != nil {
		return 0, 0, "", err
	}
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			_ = os.Remove(temporary)
		}
	}()
	hasher := sha256.New()
	multi := io.MultiWriter(file, hasher)
	encoder, err := zstd.NewWriter(multi,
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(10)),
		zstd.WithEncoderCRC(true),
	)
	if err != nil {
		return 0, 0, "", err
	}
	tarWriter := tar.NewWriter(encoder)
	walkErr := filepath.WalkDir(payloadDir, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(payloadDir, current)
		if err != nil || relative == "." {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			// 아카이브 형식은 처음부터 끝까지 일반 파일과 디렉터리만 지원하고(푸는 쪽은 다른 유형이면 바로 오류),
			// 심볼릭 링크는 되돌릴 수 없다. 패키지 전체를 실패시키지 않고 건너뛴다. 링크 대상을 읽지 않으며(lstat, 디렉터리 트리를 벗어나지 않음),
			// symlink 항목도 쓰지 않는다. 링크가 트리 안을 가리키면 대상 파일 자체는 여전히 따로 순회해 아카이브한다.
			log.Printf("[task-archive] 심볼릭 링크를 건너뜁니다(아카이브는 지원하지 않으며 다른 파일에는 영향 없음): %s", current)
			return nil
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(relative)
		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		originalSize += info.Size()
		input, err := os.Open(current)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tarWriter, input)
		closeErr := input.Close()
		return errors.Join(copyErr, closeErr)
	})
	if walkErr != nil {
		_ = tarWriter.Close()
		_ = encoder.Close()
		return 0, 0, "", walkErr
	}
	if err := tarWriter.Close(); err != nil {
		_ = encoder.Close()
		return 0, 0, "", err
	}
	if err := encoder.Close(); err != nil {
		return 0, 0, "", err
	}
	if err := file.Sync(); err != nil {
		return 0, 0, "", err
	}
	if err := file.Close(); err != nil {
		return 0, 0, "", err
	}
	stat, err := os.Stat(temporary)
	if err != nil {
		return 0, 0, "", err
	}
	compressedSize = stat.Size()
	checksum = hex.EncodeToString(hasher.Sum(nil))
	if err := os.Rename(temporary, path); err != nil {
		return 0, 0, "", err
	}
	if err := os.Chmod(path, archiveFileMode); err != nil {
		return 0, 0, "", err
	}
	committed = true
	return originalSize, compressedSize, checksum, nil
}

func extractTaskArchivePackage(path, expectedSHA, destination string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if expectedSHA != "" {
		hasher := sha256.New()
		if _, err := io.Copy(hasher, file); err != nil {
			return err
		}
		if hex.EncodeToString(hasher.Sum(nil)) != strings.ToLower(expectedSHA) {
			return errors.New("task archive checksum mismatch")
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return err
		}
	}
	decoder, err := zstd.NewReader(file)
	if err != nil {
		return err
	}
	defer decoder.Close()
	tarReader := tar.NewReader(decoder)
	if err := os.MkdirAll(destination, archiveDirMode); err != nil {
		return err
	}
	var entries int
	var total int64
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		entries++
		if entries > maxArchiveFiles || header.Size < 0 || total+header.Size > maxArchiveBytes {
			return errors.New("task archive exceeds extraction safety limits")
		}
		total += header.Size
		clean := filepath.Clean(filepath.FromSlash(header.Name))
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe task archive path %q", header.Name)
		}
		target := filepath.Join(destination, clean)
		if relative, err := filepath.Rel(destination, target); err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe task archive target %q", header.Name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, archiveDirMode); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), archiveDirMode); err != nil {
				return err
			}
			output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fs.FileMode(header.Mode)&0o777)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(output, tarReader, header.Size)
			closeErr := output.Close()
			if err := errors.Join(copyErr, closeErr); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported task archive entry type %d", header.Typeflag)
		}
	}
	return nil
}
