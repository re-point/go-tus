package tus

import (
	"bytes"
)

type Uploader struct {
	client     *Client
	url        string
	upload     *Upload
	aborted    bool
	uploadSubs []chan Upload
	notifyChan chan bool
}

// Subscribes to progress updates.
func (u *Uploader) NotifyUploadProgress(c chan Upload) {
	u.uploadSubs = append(u.uploadSubs, c)
}

// Abort aborts the upload process.
// It doens't abort the current chunck, only the remaining.
func (u *Uploader) Abort() {
	u.aborted = true
}

// IsAborted returns true if the upload was aborted.
func (u *Uploader) IsAborted() bool {
	return u.aborted
}

// Url returns the upload url.
func (u *Uploader) Url() string {
	return u.url
}


// Upload uploads the entire body to the server.
func (u *Uploader) Upload() error {
	for !u.upload.Finished() && !u.aborted {
		err := u.UploadChunck()

		if err != nil {
			return err
		}
	}

	return nil
}

// UploadChunck uploads a single chunck.
func (u *Uploader) UploadChunck() error {
	data := make([]byte, u.client.Config.ChunkSize)

	_, err := u.upload.stream.Seek(u.upload.Offset(), 0)

	if err != nil {
		return err
	}

	size, err := u.upload.stream.Read(data)

	if err != nil {
		return err
	}

	body := bytes.NewBuffer(data[:size])

	newOffset, err := u.client.uploadChunck(u.url, body, int64(size), *u.upload)

	if err != nil {
		return err
	}

	u.upload.SetOffset(newOffset)

	u.notifyChan <- true

	return nil
}

// Waits for a signal to broadcast to all subscribers
func (u *Uploader) broadcastProgress() {
	for range u.notifyChan {
		for _, c := range u.uploadSubs {
			c <- *u.upload
		}
	}
}

// NewUploader creates a new Uploader.
func NewUploader(client *Client, url string, upload *Upload) *Uploader {
	notifyChan := make(chan bool)

	uploader := &Uploader{
		client,
		url,
		upload,
		false,
		nil,
		notifyChan,
	}

	go uploader.broadcastProgress()

	return uploader
}
