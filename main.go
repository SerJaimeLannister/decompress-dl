package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"embed" // <-- NEW: Import embed package
	"fmt"
	"html/template" // <-- NEW: Import html/template
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	ffmpeg "github.com/u2takey/ffmpeg-go"
)

//go:embed templates/* <-- NEW: Directive to embed all files in templates/
var templatesFS embed.FS // <-- NEW: Variable to hold the embedded files

// --- Data Structures ---
type JobStatus string

const (
	StatusPending    JobStatus = "pending"
	StatusProcessing JobStatus = "processing"
	StatusCompleted  JobStatus = "completed"
	StatusFailed     JobStatus = "failed"
)

type Job struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Status    JobStatus `json:"status"`
	Details   string    `json:"details"`
	ResultURL string    `json:"result_url,omitempty"` // URL to download the result
}

var jobStore = sync.Map{}

// --- Helper Functions ---

func updateJob(id string, status JobStatus, details string, resultURL string) {
	val, ok := jobStore.Load(id)
	if !ok {
		return
	}
	job := val.(Job)
	job.Status = status
	job.Details = details
	if resultURL != "" {
		job.ResultURL = resultURL
	}
	jobStore.Store(id, job)
}

// Security: Prevent Zip Slip
func sanitizePath(dest, path string) (string, error) {
	fpath := filepath.Join(dest, path)
	if !strings.HasPrefix(fpath, filepath.Clean(dest)+string(os.PathSeparator)) {
		return "", fmt.Errorf("illegal file path: %s", path)
	}
	return fpath, nil
}

// --- Decompression Logic ---
func unzipSource(source, dest string) error {
	r, err := zip.OpenReader(source)
	if err != nil {
		return err
	}
	defer r.Close()
	os.MkdirAll(dest, 0755)

	for _, f := range r.File {
		fpath, err := sanitizePath(dest, f.Name)
		if err != nil {
			return err
		}

		if f.FileInfo().IsDir() {
			os.MkdirAll(fpath, os.ModePerm)
			continue
		}
		if err = os.MkdirAll(filepath.Dir(fpath), os.ModePerm); err != nil {
			return err
		}
		outFile, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			outFile.Close()
			return err
		}
		_, err = io.Copy(outFile, rc)
		outFile.Close()
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func untarSource(source, dest string) error {
	f, err := os.Open(source)
	if err != nil {
		return err
	}
	defer f.Close()
	gzr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gzr.Close()
	tr := tar.NewReader(gzr)
	os.MkdirAll(dest, 0755)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		fpath, err := sanitizePath(dest, header.Name)
		if err != nil {
			return err
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(fpath, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(fpath), 0755); err != nil {
				return err
			}
			outFile, err := os.Create(fpath)
			if err != nil {
				return err
			}
			if _, err := io.Copy(outFile, tr); err != nil {
				outFile.Close()
				return err
			}
			outFile.Close()
		}
	}
	return nil
}

func unrarSource(source, dest string) error {
	os.MkdirAll(dest, 0755)
	cmd := exec.Command("unrar", "x", "-y", source, dest+string(os.PathSeparator))
	return cmd.Run()
}

// --- Download & Zip Logic ---
func downloadFile(url string, customName string, destFolder string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	filename := customName
	if filename == "" {
		if cd := resp.Header.Get("Content-Disposition"); cd != "" {
			_, params, err := mime.ParseMediaType(cd)
			if err == nil {
				filename = params["filename"]
			}
		}
		if filename == "" {
			filename = filepath.Base(resp.Request.URL.Path)
		}
		if filename == "" || filename == "." || filename == "/" {
			filename = "download_" + uuid.New().String()
		}
	}

	filename = filepath.Base(filename)
	finalPath := filepath.Join(destFolder, filename)
	out, err := os.Create(finalPath)
	if err != nil {
		return "", err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return finalPath, err
}

func zipFile(sourcePath string, destFolder string) (string, error) {
	filename := filepath.Base(sourcePath)
	zipName := fmt.Sprintf("%s_%s.zip", strings.TrimSuffix(filename, filepath.Ext(filename)), uuid.New().String()[:8])
	zipPath := filepath.Join(destFolder, zipName)
	outFile, err := os.Create(zipPath)
	if err != nil {
		return "", err
	}
	defer outFile.Close()
	w := zip.NewWriter(outFile)
	defer w.Close()
	srcFile, err := os.Open(sourcePath)
	if err != nil {
		return "", err
	}
	defer srcFile.Close()
	f, err := w.Create(filename)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(f, srcFile)
	return zipPath, nil
}

// --- REMUX LOGIC (In-Place) ---
func remuxFile(relativePath string, container string, outputName string) (string, error) {
	// Full path to source
	sourcePath := filepath.Join("./downloads", relativePath)

	// Determine directory of the source file
	sourceDir := filepath.Dir(sourcePath)

	// Determine Output Filename
	filename := filepath.Base(sourcePath)
	var finalName string
	if outputName != "" {
		if !strings.HasSuffix(outputName, "."+container) {
			finalName = outputName + "." + container
		} else {
			finalName = outputName
		}
	} else {
		finalName = strings.TrimSuffix(filename, filepath.Ext(filename)) + "." + container
	}

	// Output goes to same directory as source
	outPath := filepath.Join(sourceDir, finalName)

	// FFmpeg command: -c copy (Remuxing, no re-encoding)
	err := ffmpeg.Input(sourcePath).
		Output(outPath, ffmpeg.KwArgs{"c": "copy"}).
		OverWriteOutput().
		Run()

	if err != nil {
		return "", fmt.Errorf("ffmpeg error: %v", err)
	}

	// Return the relative path so the frontend can find it
	relPath, _ := filepath.Rel("./downloads", outPath)
	return relPath, nil
}

// --- Worker ---
func processJob(job Job, payload map[string]interface{}) {
	updateJob(job.ID, StatusProcessing, "Starting...", "")

	var err error
	var resultPath string // Relative path inside ./downloads

	switch job.Type {
	case "download":
		url := payload["url"].(string)
		customName := payload["custom_name"].(string)
		absPath, e := downloadFile(url, customName, "./downloads")
		err = e
		if err == nil {
			resultPath = filepath.Base(absPath)
			if val, ok := payload["auto_zip"]; ok && val.(bool) {
				updateJob(job.ID, StatusProcessing, "Zipping...", "")
				// Zip goes to downloads too for simplicity now? Or keep output?
				// Let's keep existing zip logic but return that URL
				zipP, zErr := zipFile(absPath, "./downloads")
				if zErr == nil {
					resultPath = filepath.Base(zipP)
				}
			}
		}

	case "remux":
		// Payload: "filename" is actually the relative path (e.g. "360p/video.mp4")
		relPath := payload["filename"].(string)
		container := payload["container"].(string)
		customOut, _ := payload["custom_out"].(string)

		resultPath, err = remuxFile(relPath, container, customOut)

	case "extract":
		relPath := payload["filename"].(string)
		sourcePath := filepath.Join("./downloads", relPath)
		// Extract to: downloads/folder_name/
		destFolder := strings.TrimSuffix(sourcePath, filepath.Ext(sourcePath))

		ext := strings.ToLower(filepath.Ext(sourcePath))
		updateJob(job.ID, StatusProcessing, "Extracting...", "")

		if ext == ".zip" {
			err = unzipSource(sourcePath, destFolder)
		} else if ext == ".gz" || strings.HasSuffix(sourcePath, ".tar.gz") {
			err = untarSource(sourcePath, destFolder)
		} else if ext == ".rar" {
			err = unrarSource(sourcePath, destFolder)
		} else {
			err = fmt.Errorf("unsupported format: %s", ext)
		}
		// Result path for extraction is the folder name
		if err == nil {
			resultPath, _ = filepath.Rel("./downloads", destFolder)
		}
	}

	if err != nil {
		updateJob(job.ID, StatusFailed, err.Error(), "")
	} else {
		// Public URL is /raw/ + relative path
		publicURL := "/raw/" + resultPath
		updateJob(job.ID, StatusCompleted, "Done", publicURL)
	}
}

// --- Main ---

func main() {
	r := gin.Default()
	
	// NEW: Load templates from the embedded filesystem
	tmpl := template.Must(template.New("").ParseFS(templatesFS, "templates/*"))
	r.SetHTMLTemplate(tmpl)

	os.MkdirAll("./downloads", 0755)

	// Serve downloads folder directly
	r.StaticFS("/raw", http.Dir("./downloads"))

	r.GET("/", func(c *gin.Context) { c.HTML(http.StatusOK, "index.html", nil) })

	// List files with dir support
	r.GET("/api/files", func(c *gin.Context) {
		reqDir := c.DefaultQuery("dir", "")
		baseDir := "./downloads"
		targetDir := filepath.Join(baseDir, reqDir)

		if !strings.HasPrefix(filepath.Clean(targetDir), filepath.Clean(baseDir)) {
			c.JSON(400, gin.H{"error": "Invalid directory"})
			return
		}

		entries, err := os.ReadDir(targetDir)
		if err != nil {
			c.JSON(500, gin.H{"error": "Cannot read directory"})
			return
		}

		var files []gin.H
		if reqDir != "" {
			parent := filepath.Dir(reqDir)
			if parent == "." {
				parent = ""
			}
			files = append(files, gin.H{"name": "..", "type": "dir", "path": parent})
		}

		for _, e := range entries {
			info, _ := e.Info()
			fileType := "file"
			if e.IsDir() {
				fileType = "dir"
			}
			relPath := filepath.Join(reqDir, e.Name())
			files = append(files, gin.H{
				"name": e.Name(), "size": info.Size(), "type": fileType, "path": relPath,
			})
		}
		c.JSON(http.StatusOK, gin.H{"files": files, "current": reqDir})
	})

	r.DELETE("/api/files", func(c *gin.Context) {
		relativePath := c.Query("path")
		if relativePath == "" {
			c.JSON(400, gin.H{"error": "path required"})
			return
		}
		targetPath := filepath.Join("./downloads", relativePath)
		if !strings.HasPrefix(filepath.Clean(targetPath), filepath.Clean("./downloads")) {
			c.JSON(400, gin.H{"error": "Invalid path"})
			return
		}
		err := os.RemoveAll(targetPath)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"status": "deleted"})
	})

	r.POST("/api/download", func(c *gin.Context) {
		var req struct {
			URL        string `json:"url"`
			CustomName string `json:"custom_name"`
			AutoZip    bool   `json:"auto_zip"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		jobID := uuid.New().String()
		job := Job{ID: jobID, Type: "download", Status: StatusPending}
		jobStore.Store(jobID, job)
		go processJob(job, map[string]interface{}{"url": req.URL, "custom_name": req.CustomName, "auto_zip": req.AutoZip})
		c.JSON(202, gin.H{"job_id": jobID})
	})

	r.POST("/api/remux", func(c *gin.Context) {
		var req struct {
			Filename  string `json:"filename"`
			Container string `json:"container"`
			CustomOut string `json:"custom_out"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		jobID := uuid.New().String()
		job := Job{ID: jobID, Type: "remux", Status: StatusPending}
		jobStore.Store(jobID, job)
		go processJob(job, map[string]interface{}{"filename": req.Filename, "container": req.Container, "custom_out": req.CustomOut})
		c.JSON(202, gin.H{"job_id": jobID})
	})

	r.POST("/api/extract", func(c *gin.Context) {
		var req struct {
			Filename string `json:"filename"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		jobID := uuid.New().String()
		job := Job{ID: jobID, Type: "extract", Status: StatusPending}
		jobStore.Store(jobID, job)
		go processJob(job, map[string]interface{}{"filename": req.Filename})
		c.JSON(202, gin.H{"job_id": jobID})
	})

	r.GET("/api/job/:id", func(c *gin.Context) {
		id := c.Param("id")
		if val, ok := jobStore.Load(id); ok {
			c.JSON(200, val)
		} else {
			c.JSON(404, gin.H{"error": "Not found"})
		}
	})

	fmt.Println("Running on http://localhost:8080")
	r.Run("0.0.0.0:8080")
}
