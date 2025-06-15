package teldrive

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/alist-org/alist/v3/drivers/base"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/go-resty/resty/v2"
)

const (
	minChunkSize = 1
	maxChunkSize = 2000
)

type Teldrive struct {
	model.Storage
	Addition
	client       *resty.Client
	userId       int64
	rootFolderID string
}

func (d *Teldrive) Config() driver.Config {
	return config
}

func (d *Teldrive) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *Teldrive) Init(ctx context.Context) error {
	if d.Cookie == "" || !strings.HasPrefix(d.Cookie, "access_token=") {
		return fmt.Errorf("cookie must start with 'access_token='")
	}

	if d.UploadConcurrency == 0 {
		d.UploadConcurrency = 4
	}

	if d.ChunkSize == 0 {
		d.ChunkSize = 10
	}

	if d.ChunkSize < minChunkSize {
		return fmt.Errorf("chunk size must be at least %d MiB", minChunkSize)
	}

	if d.ChunkSize > maxChunkSize {
		return fmt.Errorf("chunk size must be at most %d MiB", maxChunkSize)
	}

	d.Address = strings.TrimSuffix(d.Address, "/")

	d.client = base.NewRestyClient()
	d.client.SetCookie(&http.Cookie{
		Name:  "access_token",
		Value: strings.TrimPrefix(d.Cookie, "access_token="),
	})
	d.client.SetBaseURL(d.Address)

	// Get session info
	var session Session
	resp, err := d.client.R().SetResult(&session).Get("/api/auth/session")
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}
	if resp.StatusCode() != 200 {
		return fmt.Errorf("failed to get session: %s", resp.String())
	}
	if session.UserId == 0 {
		return errors.New("invalid session")
	}

	d.userId = session.UserId

	// Get root folder ID
	rootID, err := d.getRootID(ctx)
	if err != nil {
		return err
	}
	d.rootFolderID = rootID

	op.MustSaveDriverStorage(d)
	return nil
}

func (d *Teldrive) Drop(ctx context.Context) error {
	return nil
}

func (d *Teldrive) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	files, err := d.getFiles(ctx, dir.GetID())
	if err != nil {
		return nil, err
	}

	return utils.SliceConvert(files, func(src FileInfo) (model.Obj, error) {
		return fileToObj(src), nil
	})
}

func (d *Teldrive) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	url := fmt.Sprintf("%s/api/files/%s/%s", d.Address, file.GetID(), url.QueryEscape(file.GetName()))
	if !d.Addition.EncryptFiles {
		url += "?download=1"
	}

	return &model.Link{
		URL: url,
	}, nil
}

func (d *Teldrive) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) (model.Obj, error) {
	req := CreateFileRequest{
		Name:     dirName,
		Type:     "folder",
		ParentId: parentDir.GetID(),
	}

	var fileInfo FileInfo
	resp, err := d.client.R().
		SetBody(req).
		SetResult(&fileInfo).
		Post("/api/files")

	if err != nil {
		return nil, err
	}
	if resp.StatusCode() != 200 {
		return nil, fmt.Errorf("failed to create directory: %s", resp.String())
	}

	return fileToObj(fileInfo), nil
}

func (d *Teldrive) Move(ctx context.Context, srcObj, dstDir model.Obj) (model.Obj, error) {
	req := MoveFileRequest{
		Destination: dstDir.GetID(),
		Files:       []string{srcObj.GetID()},
	}

	resp, err := d.client.R().
		SetBody(req).
		Post("/api/files/move")

	if err != nil {
		return nil, err
	}
	if resp.StatusCode() != 200 {
		return nil, fmt.Errorf("failed to move: %s", resp.String())
	}

	// Return a new object with updated properties
	return &model.ObjThumb{
		Object: model.Object{
			ID:       srcObj.GetID(),
			Name:     srcObj.GetName(),
			Size:     srcObj.GetSize(),
			Modified: srcObj.ModTime(),
			IsFolder: srcObj.IsDir(),
		},
		Thumbnail: model.Thumbnail{},
	}, nil
}

func (d *Teldrive) Rename(ctx context.Context, srcObj model.Obj, newName string) (model.Obj, error) {
	req := UpdateFileInformation{
		Name: newName,
	}

	resp, err := d.client.R().
		SetBody(req).
		Patch(fmt.Sprintf("/api/files/%s", srcObj.GetID()))

	if err != nil {
		return nil, err
	}
	if resp.StatusCode() != 200 {
		return nil, fmt.Errorf("failed to rename: %s", resp.String())
	}

	newObj := *srcObj.(*model.ObjThumb)
	newObj.Name = newName
	return &newObj, nil
}

func (d *Teldrive) Copy(ctx context.Context, srcObj, dstDir model.Obj) (model.Obj, error) {
	req := CopyFile{
		Newname:     srcObj.GetName(),
		Destination: dstDir.GetID(),
		ModTime:     srcObj.ModTime(),
	}

	var fileInfo FileInfo
	resp, err := d.client.R().
		SetBody(req).
		SetResult(&fileInfo).
		Post(fmt.Sprintf("/api/files/%s/copy", srcObj.GetID()))

	if err != nil {
		return nil, err
	}
	if resp.StatusCode() != 200 {
		return nil, fmt.Errorf("failed to copy: %s", resp.String())
	}

	return fileToObj(fileInfo), nil
}

func (d *Teldrive) Remove(ctx context.Context, obj model.Obj) error {
	req := RemoveFileRequest{
		Files: []string{obj.GetID()},
	}

	resp, err := d.client.R().
		SetBody(req).
		Post("/api/files/delete")

	if err != nil {
		return err
	}
	if resp.StatusCode() != 200 {
		return fmt.Errorf("failed to remove: %s", resp.String())
	}

	return nil
}

func (d *Teldrive) Put(ctx context.Context, dstDir model.Obj, stream model.FileStreamer, up driver.UpdateProgress) (model.Obj, error) {
	if stream.GetSize() < 0 {
		return nil, errors.New("teldrive can't upload files with unknown size")
	}

	uploadInfo, err := d.prepareUpload(ctx, dstDir, stream)
	if err != nil {
		return nil, err
	}

	if stream.GetSize() > 0 {
		err = d.uploadMultipart(ctx, stream, uploadInfo, up)
		if err != nil {
			return nil, err
		}
	}

	return d.createFile(ctx, stream, uploadInfo)
}

// Helper functions

func (d *Teldrive) getRootID(ctx context.Context) (string, error) {
	resp, err := d.client.R().
		SetQueryParams(map[string]string{
			"parentId":  "nil",
			"operation": "find",
			"name":      "root",
			"type":      "folder",
		}).
		Get("/api/files")

	if err != nil {
		return "", err
	}

	var result ReadMetadataResponse
	if err := utils.Json.Unmarshal(resp.Body(), &result); err != nil {
		return "", err
	}

	if len(result.Files) == 0 {
		return "", fmt.Errorf("couldn't find root directory")
	}

	return result.Files[0].Id, nil
}

func (d *Teldrive) getFiles(ctx context.Context, parentID string) ([]FileInfo, error) {
	resp, err := d.client.R().
		SetQueryParams(map[string]string{
			"parentId":  parentID,
			"limit":     "500",
			"sort":      "id",
			"operation": "list",
			"page":      "1",
		}).
		Get("/api/files")

	if err != nil {
		return nil, err
	}

	var result ReadMetadataResponse
	if err := utils.Json.Unmarshal(resp.Body(), &result); err != nil {
		return nil, err
	}

	return result.Files, nil
}

func (d *Teldrive) prepareUpload(ctx context.Context, dstDir model.Obj, stream model.FileStreamer) (*UploadInfo, error) {
	fileName := stream.GetName()
	uploadID := getMD5Hash(fmt.Sprintf("%s:%s:%d:%d", dstDir.GetID(), fileName, stream.GetSize(), d.userId))

	chunkSize := d.ChunkSize * 1024 * 1024 // Convert MiB to bytes
	totalChunks := stream.GetSize() / chunkSize
	if stream.GetSize()%chunkSize != 0 {
		totalChunks++
	}

	channelID := d.ChannelID
	encryptFile := d.EncryptFiles

	return &UploadInfo{
		ExistingChunks: make(map[int]PartFile),
		UploadID:       uploadID,
		ChannelID:      channelID,
		EncryptFile:    encryptFile,
		ChunkSize:      chunkSize,
		TotalChunks:    totalChunks,
		FileName:       fileName,
		Dir:            dstDir.GetID(),
	}, nil
}

func (d *Teldrive) uploadMultipart(ctx context.Context, stream model.FileStreamer, uploadInfo *UploadInfo, up driver.UpdateProgress) error {
	var partsToCommit []PartFile
	var uploadedSize int64

	totalChunks := int(uploadInfo.TotalChunks)

	for chunkNo := 1; chunkNo <= totalChunks; chunkNo++ {
		n := uploadInfo.ChunkSize
		if chunkNo == totalChunks {
			n = stream.GetSize() - uploadedSize
		}

		chunkName := uploadInfo.FileName
		if totalChunks > 1 {
			chunkName = fmt.Sprintf("%s.part.%03d", chunkName, chunkNo)
		}

		// Read chunk data
		chunkData := make([]byte, n)
		_, err := io.ReadFull(stream, chunkData)
		if err != nil && err != io.EOF {
			return err
		}

		uploadURL := "/api/uploads/" + uploadInfo.UploadID
		if d.UploadHost != "" {
			uploadURL = d.UploadHost + uploadURL
		}

		var partInfo PartFile
		resp, err := d.client.R().
			SetBody(chunkData).
			SetHeader("Content-Type", "application/octet-stream").
			SetQueryParams(map[string]string{
				"partName":  chunkName,
				"fileName":  uploadInfo.FileName,
				"partNo":    strconv.Itoa(chunkNo),
				"channelId": strconv.FormatInt(uploadInfo.ChannelID, 10),
				"encrypted": strconv.FormatBool(uploadInfo.EncryptFile),
			}).
			SetResult(&partInfo).
			Post(uploadURL)

		if err != nil {
			return err
		}
		if resp.StatusCode() != 200 {
			return fmt.Errorf("failed to upload chunk %d: %s", chunkNo, resp.String())
		}

		uploadedSize += n
		partsToCommit = append(partsToCommit, partInfo)

		if up != nil {
			up(float64(uploadedSize) / float64(stream.GetSize()) * 100)
		}
	}

	fileChunks := make([]FilePart, len(partsToCommit))
	for i, part := range partsToCommit {
		fileChunks[i] = FilePart{ID: part.PartId, Salt: part.Salt}
	}

	uploadInfo.FileChunks = fileChunks
	return nil
}

func (d *Teldrive) createFile(ctx context.Context, stream model.FileStreamer, uploadInfo *UploadInfo) (model.Obj, error) {
	req := CreateFileRequest{
		Name:      uploadInfo.FileName,
		Type:      "file",
		ParentId:  uploadInfo.Dir,
		MimeType:  utils.GetMimeType(uploadInfo.FileName),
		Size:      stream.GetSize(),
		Parts:     uploadInfo.FileChunks,
		ChannelID: uploadInfo.ChannelID,
		Encrypted: uploadInfo.EncryptFile,
		ModTime:   time.Now().UTC(),
	}

	var fileInfo FileInfo
	resp, err := d.client.R().
		SetBody(req).
		SetResult(&fileInfo).
		Post("/api/files")

	if err != nil {
		return nil, err
	}
	if resp.StatusCode() != 200 {
		return nil, fmt.Errorf("failed to create file: %s", resp.String())
	}

	// Clean up upload
	if stream.GetSize() > 0 {
		d.client.R().Delete("/api/uploads/" + uploadInfo.UploadID)
	}

	return fileToObj(fileInfo), nil
}

func fileToObj(f FileInfo) model.Obj {
	return &model.ObjThumb{
		Object: model.Object{
			ID:       f.Id,
			Name:     f.Name,
			Size:     f.Size,
			Modified: f.ModTime,
			IsFolder: f.Type == "folder",
		},
		Thumbnail: model.Thumbnail{},
	}
}

func getMD5Hash(text string) string {
	hash := md5.Sum([]byte(text))
	return hex.EncodeToString(hash[:])
}

var _ driver.Driver = (*Teldrive)(nil)
