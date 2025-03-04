package tus

import (
	"io"
	"io/ioutil"
	"log"
	"net/http"
	netUrl "net/url"
	"strconv"
)

const (
	ProtocolVersion = "1.0.0"
)

// Client represents the tus client.
// You can use it in goroutines to create parallels uploads.
type Client struct {
	Config  *Config
	Url     string
	Version string
	Header  http.Header

	client *http.Client
}

// NewClient creates a new tus client.
func NewClient(url string, config *Config) (*Client, error) {
	if config == nil {
		config = DefaultConfig()
	} else {
		if err := config.Validate(); err != nil {
			return nil, err
		}
	}

	if config.Header == nil {
		config.Header = make(http.Header)
	}

	if config.HttpClient == nil {
		config.HttpClient = &http.Client{}
	}

	return &Client{
		Config:  config,
		Url:     url,
		Version: ProtocolVersion,
		Header:  config.Header,

		client: config.HttpClient,
	}, nil
}

func (c *Client) Do(req *http.Request) (*http.Response, error) {
	for k, v := range c.Header {
		req.Header[k] = v
	}

	req.Header.Set("Tus-Resumable", ProtocolVersion)

	return c.client.Do(req)
}

// CreateUpload creates a new upload in the server.
func (c *Client) CreateUpload(u *Upload) (*Uploader, error) {
	if u == nil {
		return nil, ErrNilUpload
	}

	if c.Config.Resume && len(u.Fingerprint) == 0 {
		return nil, ErrFingerprintNotSet
	}

	req, err := http.NewRequest("POST", c.Url, nil)

	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Length", "0")
	req.Header.Set("Upload-Length", strconv.FormatInt(u.totalSize, 10))
	req.Header.Set("Upload-Metadata", u.EncodedMetadata())

	res, err := c.Do(req)

	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	switch res.StatusCode {
	case 201:
		url := res.Header.Get("Location")

		baseUrl, err := netUrl.Parse(c.Url)
		if err != nil {
			return nil, ErrUrlNotRecognized
		}

		newUrl, err := netUrl.Parse(url)
		if err != nil {
			return nil, ErrUrlNotRecognized
		}
		if newUrl.Scheme == "" {
			newUrl.Scheme = baseUrl.Scheme
			url = newUrl.String()
		}

		if c.Config.Resume {
			c.Config.Store.Set(u.Fingerprint, url)
		}

		return NewUploader(c, url, u), nil
	case 412:
		return nil, ErrVersionMismatch
	case 413:
		return nil, ErrLargeUpload
	default:
		return nil, newClientError(res)
	}
}

// ResumeUpload resumes the upload if already created, otherwise it will return an error.
func (c *Client) ResumeUpload(u *Upload) (*Uploader, error) {
	if u == nil {
		return nil, ErrNilUpload
	}

	if !c.Config.Resume {
		return nil, ErrResumeNotEnabled
	} else if len(u.Fingerprint) == 0 {
		return nil, ErrFingerprintNotSet
	}

	url, found := c.Config.Store.Get(u.Fingerprint)

	if !found {
		return nil, ErrUploadNotFound
	}

	offset, err := c.getUploadOffset(url, u.totalSize)
	if err != nil {
		return nil, err
	}

	u.SetOffset(offset)

	return NewUploader(c, url, u), nil
}

// CreateOrResumeUpload resumes the upload if already created or creates a new upload in the server.
func (c *Client) CreateOrResumeUpload(u *Upload) (*Uploader, error) {
	if u == nil {
		return nil, ErrNilUpload
	}

	uploader, err := c.ResumeUpload(u)

	if err == nil {
		return uploader, err
	} else if (err == ErrResumeNotEnabled) || (err == ErrUploadNotFound) {
		return c.CreateUpload(u)
	}

	return nil, err
}

func  (c *Client) TerminateUpload(u *Upload) error {
	if !c.Config.Resume {
		return ErrResumeNotEnabled
	}

	url, found := c.Config.Store.Get(u.Fingerprint)
	if !found {
		return ErrUploadNotFound
	}

	req, err := http.NewRequest("DELETE", url, nil)

	if err != nil {
		return err
	}

	res, err := c.Do(req)

	if err != nil {
		return err
	}
	defer res.Body.Close()

	switch res.StatusCode {
	case 204:
		return nil
	case 403, 404, 410:
		// file doesn't exists.
		return ErrUploadNotFound
	case 412:
		return ErrVersionMismatch
	default:
		return newClientError(res)
	}
}

func (c *Client) uploadChunck(url string, body io.Reader, chunkSize int64, u Upload) (int64, error) {
	var method string
	
	if u.totalSize <  u.offset + chunkSize {
		log.Printf("compute | invalid serverside: expected %d, actual %d", u.totalSize, u.offset + chunkSize)
		return -1, ErrServerSizeMismatch
	}

	if !c.Config.OverridePatchMethod {
		method = "PATCH"
	} else {
		method = "POST"
	}

	req, err := http.NewRequest(method, url, body)

	if err != nil {
		return -1, err
	}

	req.Header.Set("Content-Type", "application/offset+octet-stream")
	req.Header.Set("Content-Length", strconv.FormatInt(chunkSize, 10))
	req.Header.Set("Upload-Offset", strconv.FormatInt(u.offset, 10))

	if c.Config.OverridePatchMethod {
		req.Header.Set("X-HTTP-Method-Override", "PATCH")
	}

	res, err := c.Do(req)

	if err != nil {
		return -1, err
	}
	defer res.Body.Close()

	switch res.StatusCode {
	case 204:
		if newOffset, err := strconv.ParseInt(res.Header.Get("Upload-Offset"), 10, 64); err == nil {
			return newOffset, nil
		} else {
			return -1, err
		}
	case 409:
		return -1, ErrOffsetMismatch
	case 412:
		return -1, ErrVersionMismatch
	case 413:
		return -1, ErrLargeUpload
	default:
		return -1, newClientError(res)
	}
}

func (c *Client) getUploadOffset(url string, totalSize int64) (int64, error) {
	req, err := http.NewRequest("HEAD", url, nil)

	if err != nil {
		return -1, err
	}

	res, err := c.Do(req)

	if err != nil {
		return -1, err
	}
	defer res.Body.Close()

	switch res.StatusCode {
	case 200:
		offset, err := strconv.ParseInt(res.Header.Get("Upload-Offset"), 10, 64)
		if err != nil {
			return -1, err
		}

		serverLength, err := strconv.ParseInt(res.Header.Get("Upload-Length"), 10, 64)
		if err != nil {
			return -1, err
		}
		
		if serverLength != totalSize {
			log.Printf("compare | invalid serverside: expected %d, actual %d", totalSize, serverLength)
			return -1, ErrServerSizeMismatch
		}
		
		return offset, nil
	case 403, 404, 410:
		// file doesn't exists.
		return -1, ErrUploadNotFound
	case 412:
		return -1, ErrVersionMismatch
	default:
		return -1, newClientError(res)
	}
}

func newClientError(res *http.Response) ClientError {
	body, _ := ioutil.ReadAll(res.Body)
	return ClientError{
		Code: res.StatusCode,
		Body: body,
	}
}