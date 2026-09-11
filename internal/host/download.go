package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/MOVEI144/RunnerLoom/internal/core"
)

type ImageDownloadReport struct {
	Digest          string `json:"digest"`
	Path            string `json:"path"`
	QuarantinedPath string `json:"quarantinedPath,omitempty"`
	ResumedFrom     int64  `json:"resumedFrom"`
	Downloaded      int64  `json:"downloaded"`
	TotalBytes      int64  `json:"totalBytes"`
	Complete        bool   `json:"complete"`
	PartialRetain   bool   `json:"partialRetained"`
}

type contentRange struct {
	Start int64
	End   int64
	Total int64
}

func parseContentRange(value string) (contentRange, error) {
	var out contentRange
	if !strings.HasPrefix(value, "bytes ") {
		return out, errors.New("missing byte content range")
	}
	span, totalText, ok := strings.Cut(strings.TrimPrefix(value, "bytes "), "/")
	if !ok || totalText == "*" {
		return out, errors.New("content range has no total size")
	}
	startText, endText, ok := strings.Cut(span, "-")
	if !ok {
		return out, errors.New("invalid content range span")
	}
	var err error
	if out.Start, err = strconv.ParseInt(startText, 10, 64); err != nil {
		return out, errors.New("invalid content range start")
	}
	if out.End, err = strconv.ParseInt(endText, 10, 64); err != nil {
		return out, errors.New("invalid content range end")
	}
	if out.Total, err = strconv.ParseInt(totalText, 10, 64); err != nil {
		return out, errors.New("invalid content range total")
	}
	if out.Start < 0 || out.End < out.Start || out.Total <= out.End {
		return out, errors.New("inconsistent content range")
	}
	return out, nil
}

func (i *Images) verifyPath(ctx context.Context, path, digest string) error {
	actual, _, err := hashFile(path)
	if err != nil {
		return err
	}
	if actual != digest {
		return errImageDigestMismatch
	}
	return i.inspect(ctx, path)
}

func (i *Images) publishPartial(ctx context.Context, partial, final, digest string) error {
	if err := i.verifyPath(ctx, partial, digest); err != nil {
		_ = os.Remove(partial)
		return err
	}
	if _, err := os.Lstat(final); err == nil {
		return errors.New("image appeared while download was being finalized")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(partial, final); err != nil {
		return err
	}
	return syncDirectory(i.Dir)
}

// Download retrieves an authenticated content-addressed image. Interrupted
// transfers retain an owner-only partial file and resume with HTTP Range on the
// next attempt. A completed file is hash- and qcow2-verified before publication.
func (i *Images) Download(ctx context.Context, client *http.Client, sourceURL, digest string) (ImageDownloadReport, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	result := ImageDownloadReport{Digest: digest}
	name, err := imageName(digest)
	if err != nil {
		return result, err
	}
	if client == nil || sourceURL == "" {
		return result, errors.New("image download requires an HTTP client and source URL")
	}
	if i.LimitGiB < 1 || i.LimitGiB > 1048576 {
		return result, errors.New("invalid image cache limit")
	}
	if err = core.PrivateDir(i.Dir); err != nil {
		return result, err
	}
	lock, err := core.AcquireLock(i.Dir, "image-cache")
	if err != nil {
		return result, err
	}
	defer lock.Close()

	final := filepath.Join(i.Dir, name)
	result.Path = final
	if _, err = os.Lstat(final); err == nil {
		if err = i.verifyPath(ctx, final, digest); err != nil {
			if !errors.Is(err, errImageDigestMismatch) {
				return result, err
			}
			result.QuarantinedPath, err = i.quarantineDigestMismatch(final, digest)
			if err != nil {
				return result, fmt.Errorf("corrupt cache image could not be quarantined: %w", err)
			}
		} else {
			st, statErr := os.Stat(final)
			if statErr != nil {
				return result, statErr
			}
			result.TotalBytes = st.Size()
			result.Complete = true
			return result, nil
		}
	} else if !os.IsNotExist(err) {
		return result, err
	}

	partial := filepath.Join(i.Dir, ".partial-"+name)
	partialSize := int64(0)
	if st, statErr := os.Lstat(partial); statErr == nil {
		if !st.Mode().IsRegular() {
			return result, errors.New("partial image is not a regular file")
		}
		partialSize = st.Size()
	} else if !os.IsNotExist(statErr) {
		return result, statErr
	}
	availableTotal, err := cacheAvailableBytes(i.Dir, i.LimitGiB, partialSize)
	if err != nil {
		return result, err
	}
	if availableTotal <= 0 || partialSize > availableTotal {
		return result, errors.New("image cache or filesystem safety budget is exhausted")
	}
	existingPartialSize := partialSize
	retainPartial := func(cause error) (ImageDownloadReport, error) {
		result.ResumedFrom = existingPartialSize
		result.PartialRetain = existingPartialSize > 0
		return result, cause
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return result, err
	}
	expectedETag := `"` + digest + `"`
	if partialSize > 0 {
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-", partialSize))
		request.Header.Set("If-Range", expectedETag)
	}
	request.Header.Set("Accept-Encoding", "identity")
	response, err := client.Do(request)
	if err != nil {
		return retainPartial(err)
	}
	defer response.Body.Close()

	resume := false
	expectedBody := response.ContentLength
	total := response.ContentLength
	switch response.StatusCode {
	case http.StatusOK:
		partialSize = 0
	case http.StatusPartialContent:
		if partialSize == 0 {
			return retainPartial(errors.New("unexpected partial image response"))
		}
		value, parseErr := parseContentRange(response.Header.Get("Content-Range"))
		if parseErr != nil || value.Start != partialSize {
			return retainPartial(errors.New("image source returned an invalid resume range"))
		}
		if response.ContentLength >= 0 && response.ContentLength != value.End-value.Start+1 {
			return retainPartial(errors.New("image resume length does not match content range"))
		}
		resume = true
		total = value.Total
		expectedBody = value.End - value.Start + 1
	case http.StatusRequestedRangeNotSatisfiable:
		if partialSize == 0 {
			return retainPartial(errors.New("image source rejected an empty resume request"))
		}
		value := response.Header.Get("Content-Range")
		if !strings.HasPrefix(value, "bytes */") {
			return retainPartial(errors.New("image source returned an invalid range rejection"))
		}
		total, err = strconv.ParseInt(strings.TrimPrefix(value, "bytes */"), 10, 64)
		if err != nil || total != partialSize {
			return retainPartial(errors.New("partial image size does not match source"))
		}
		if err = i.publishPartial(ctx, partial, final, digest); err != nil {
			return result, err
		}
		result.ResumedFrom = partialSize
		result.TotalBytes = partialSize
		result.Complete = true
		return result, nil
	default:
		return retainPartial(fmt.Errorf("image source returned HTTP %d", response.StatusCode))
	}
	if response.Header.Get("ETag") != expectedETag {
		return retainPartial(errors.New("image source ETag does not match requested digest"))
	}
	if total > availableTotal {
		return retainPartial(errors.New("image exceeds cache or filesystem safety budget"))
	}

	flags := os.O_CREATE | os.O_WRONLY
	if resume {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	file, err := os.OpenFile(partial, flags, 0600)
	if err != nil {
		return result, err
	}
	if st, statErr := file.Stat(); statErr != nil || !st.Mode().IsRegular() {
		file.Close()
		return result, errors.New("partial image path changed during download")
	}
	remaining := availableTotal - partialSize
	written, copyErr := io.Copy(file, io.LimitReader(response.Body, remaining+1))
	syncErr := file.Sync()
	closeErr := file.Close()
	result.ResumedFrom = partialSize
	result.Downloaded = written
	result.PartialRetain = true
	if copyErr != nil {
		return result, copyErr
	}
	if syncErr != nil {
		return result, syncErr
	}
	if closeErr != nil {
		return result, closeErr
	}
	if written > remaining {
		_ = os.Remove(partial)
		result.PartialRetain = false
		return result, errors.New("image exceeds cache or filesystem safety budget")
	}
	if expectedBody >= 0 && written != expectedBody {
		return result, io.ErrUnexpectedEOF
	}
	st, err := os.Stat(partial)
	if err != nil {
		return result, err
	}
	if total >= 0 && st.Size() != total {
		return result, io.ErrUnexpectedEOF
	}
	if err = i.publishPartial(ctx, partial, final, digest); err != nil {
		result.PartialRetain = false
		return result, err
	}
	result.TotalBytes = st.Size()
	result.Complete = true
	result.PartialRetain = false
	return result, nil
}
