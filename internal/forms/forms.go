// Copyright 2020 Rainer Grosskopf (KI7RMJ). All rights reserved.
// Use of this source code is governed by the MIT-license that can be
// found in the LICENSE file.

// Processes Winlink-compatible message template (aka Winlink forms)

package forms

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/la5nta/wl2k-go/fbb"
	"github.com/la5nta/wl2k-go/mailbox"

	"github.com/la5nta/pat/cfg"
	"github.com/la5nta/pat/internal/debug"
	"github.com/la5nta/pat/internal/directories"
	"github.com/la5nta/pat/internal/gpsd"
)

const formsVersionInfoURL = "https://api.getpat.io/v1/forms/standard-templates/latest"

const (
	htmlFileExt  = ".html"
	txtFileExt   = ".txt"
	replyFileExt = ".0"
)

// Manager manages the forms subsystem
type Manager struct {
	config   Config
	sequence Sequence

	// postedFormData serves as an kv-store holding intermediate data for
	// communicating form values submitted by the served HTML form files to
	// the rest of the app.
	//
	// When the web frontend POSTs the form template data, this map holds
	// the POST'ed data. Each form composer instance renders into another
	// browser tab, and has a unique instance cookie. This instance cookie
	// is the key into the map, so that we can keep the values from
	// different form authoring sessions separate from each other.
	postedFormData struct {
		mu sync.RWMutex
		m  map[string]Message
	}
}

func (m *Manager) SeqSet(v int) error {
	_, err := m.sequence.Set(int64(v))
	return err
}

// Config passes config options to the forms package
type Config struct {
	FormsPath      string
	SequencePath   string
	SequenceFormat string
	MyCall         string
	Locator        string
	AppVersion     string
	UserAgent      string
	GPSd           cfg.GPSdConfig
}

// FormFolder is a folder with forms. A tree structure with Form leaves and sub-Folder branches
type FormFolder struct {
	Name      string       `json:"name"`
	Path      string       `json:"path"`
	Version   string       `json:"version"`
	FormCount int          `json:"form_count"`
	Forms     []Template   `json:"forms"`
	Folders   []FormFolder `json:"folders"`
}

// UpdateResponse is the API response format for the upgrade forms endpoint
type UpdateResponse struct {
	NewestVersion string `json:"newestVersion"`
	Action        string `json:"action"`
}

var client = httpClient{http.Client{Timeout: 10 * time.Second}}

// NewManager instantiates the forms manager
func NewManager(conf Config) *Manager {
	_ = os.MkdirAll(conf.FormsPath, 0o755)
	retval := &Manager{
		config:   conf,
		sequence: OpenSequence(conf.SequencePath),
	}
	retval.postedFormData.m = make(map[string]Message)
	return retval
}

func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.sequence.Close()
	return nil
}

// GetFormsCatalogHandler reads all forms from config.FormsPath and writes them in the http response as a JSON object graph
// This lets the frontend present a tree-like GUI for the user to select a form for composing a message
func (m *Manager) GetFormsCatalogHandler(w http.ResponseWriter, r *http.Request) {
	formFolder, err := m.buildFormFolder()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		log.Printf("%s %s: %s", r.Method, r.URL.Path, err)
		return
	}
	_ = json.NewEncoder(w).Encode(formFolder)
}

// PostFormDataHandler handles both HTML form submissions and text-only template submissions.
// The handler detects the content type and processes accordingly, storing the results in
// the forms map for retrieval by other browser tabs.
func (m *Manager) PostFormDataHandler(mboxRoot string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		inReplyTo := r.URL.Query().Get("in-reply-to")
		templatePath := r.URL.Query().Get("template")
		if templatePath == "" {
			http.Error(w, "template query param missing", http.StatusBadRequest)
			log.Printf("template query param missing %s %s", r.Method, r.URL.Path)
			return
		}
		formInstanceKey, err := r.Cookie("forminstance")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			log.Printf("missing cookie %s %s", templatePath, r.URL)
			return
		}

		var formValues, promptResponses map[string]string
		switch contentType := r.Header.Get("Content-Type"); {
		case strings.HasPrefix(contentType, "multipart/form-data"):
			// Process HTML form submission
			if err := r.ParseMultipartForm(10e6); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			formValues = make(map[string]string, len(r.PostForm))
			for key, values := range r.PostForm {
				formValues[strings.TrimSpace(strings.ToLower(key))] = values[0]
			}
		case strings.HasPrefix(contentType, "application/json"):
			// Process JSON template submission (from builtin text-only template editor)
			var payload struct {
				Responses map[string]string `json:"responses"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			formValues = map[string]string{"templateversion": m.getFormsVersion()}
			promptResponses = payload.Responses
		default:
			http.Error(w, "unsupported content type", http.StatusBadRequest)
			return
		}

		templatePath = m.abs(templatePath)
		// Make sure we don't escape FormsPath
		if !directories.IsInPath(m.config.FormsPath, templatePath) {
			http.Error(w, fmt.Sprintf("%s escapes forms directory", templatePath), http.StatusForbidden)
			return
		}

		// Load template
		template, err := readTemplate(m.abs(templatePath), formFilesFromPath(m.config.FormsPath))
		switch {
		case os.IsNotExist(err):
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
			log.Printf("failed to parse relevant form template (%q): %v", m.rel(templatePath), err)
			return
		}

		// Load optional in-reply-to message
		var inReplyToMsg *fbb.Message
		if inReplyTo != "" {
			var err error
			inReplyToMsg, err = mailbox.OpenMessage(filepath.Join(mboxRoot, inReplyTo+mailbox.Ext))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				log.Printf("failed to load in-reply-to (%q): %v", inReplyTo, err)
				return
			}
		}

		// Build message
		msg, err := messageBuilder{
			Template:        template,
			FormValues:      formValues,
			PromptResponses: promptResponses, // This is for text-only templates only
			Interactive:     false,
			InReplyToMsg:    inReplyToMsg,
			FormsMgr:        m,
		}.build()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			log.Printf("%s %s: %s", r.Method, r.URL.Path, err)
			return
		}

		// Save the result for the front-end to retrieve
		m.postedFormData.mu.Lock()
		m.postedFormData.m[formInstanceKey.Value] = msg
		m.postedFormData.mu.Unlock()
		m.cleanupOldFormData()

		_, _ = io.WriteString(w, "<script>window.close()</script>")
	}
}

// GetFormDataHandler is the counterpart to PostFormDataHandler. Returns the form field values to the frontend
func (m *Manager) GetFormDataHandler(w http.ResponseWriter, r *http.Request) {
	formInstanceKey, err := r.Cookie("forminstance")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		log.Printf("missing cookie %s %s", formInstanceKey, r.URL)
		return
	}
	v, ok := m.GetPostedFormData(formInstanceKey.Value)
	if !ok {
		http.NotFound(w, r)
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}

// GetPostedFormData is similar to GetFormDataHandler, but used when posting the form-based message to the outbox
func (m *Manager) GetPostedFormData(key string) (Message, bool) {
	m.postedFormData.mu.RLock()
	defer m.postedFormData.mu.RUnlock()
	v, ok := m.postedFormData.m[key]
	return v, ok
}

// GetFormAssetHandler serves files referenced by HTML forms (through {FormFolder}).
//
// It's primary use case is to load stylesheets and smilar resources.
func (m *Manager) GetFormAssetHandler(w http.ResponseWriter, r *http.Request) {
	path := m.abs(r.URL.Path)
	// Make sure we don't escape FormsPath
	if !directories.IsInPath(m.config.FormsPath, path) {
		http.Error(w, fmt.Sprintf("%s escapes forms directory", path), http.StatusForbidden)
		return
	}
	http.ServeFile(w, r, path)
}

// GetTemplateDataHandler serves partially processed template content.
//
// It's primary use case is to provide template files to the text-only template editor.
func (m *Manager) GetTemplateDataHandler(mboxRoot string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		templatePath := r.URL.Query().Get("template")
		// Make sure we don't escape FormsPath
		if !directories.IsInPath(m.config.FormsPath, m.abs(templatePath)) {
			http.Error(w, fmt.Sprintf("%s escapes forms directory", templatePath), http.StatusForbidden)
			return
		}
		template, err := readTemplate(m.abs(templatePath), formFilesFromPath(m.config.FormsPath))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		// Load optional in-reply-to message
		var inReplyToMsg *fbb.Message
		if inReplyTo := r.URL.Query().Get("in-reply-to"); inReplyTo != "" {
			var err error
			inReplyToMsg, err = mailbox.OpenMessage(filepath.Join(mboxRoot, inReplyTo+mailbox.Ext))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				log.Printf("failed to load in-reply-to (%q): %v", inReplyTo, err)
				return
			}
		}

		// Open the template
		f, err := os.Open(template.Path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer f.Close()

		// Process insertion tags (for preview)
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, newTrimBomReader(f)); err != nil {
			panic(err)
		}
		text := insertionTagReplacer(m, inReplyToMsg, templatePath, "<", ">")(buf.String())

		io.WriteString(w, text)
	}
}

// GetFormTemplateHandler serves a template's HTML form (filled-in with instance values)
func (m *Manager) GetFormTemplateHandler(w http.ResponseWriter, r *http.Request) {
	templatePath := r.URL.Query().Get("template")
	if templatePath == "" {
		http.Error(w, "template query param missing", http.StatusBadRequest)
		log.Printf("template query param missing %s %s", r.Method, r.URL.Path)
		return
	}
	templatePath = m.abs(templatePath)
	// Make sure we don't escape FormsPath
	if !directories.IsInPath(m.config.FormsPath, templatePath) {
		http.Error(w, fmt.Sprintf("%s escapes forms directory", templatePath), http.StatusForbidden)
		return
	}

	template, err := readTemplate(templatePath, formFilesFromPath(m.config.FormsPath))
	switch {
	case os.IsNotExist(err):
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		log.Printf("failed to parse requested template (%q): %v", m.rel(templatePath), err)
		return
	}
	formPath := template.InputFormPath
	if formPath == "" {
		// This is a text-only template. Redirect to template editor.
		http.Redirect(w, r, "/ui/template?"+r.URL.Query().Encode(), http.StatusFound)
		return
	}

	responseText, err := m.fillFormTemplate(formPath, nil, "/api/form?"+r.URL.Query().Encode(), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		log.Printf("problem filling form template file %s %s: can't open template %s. Err: %s", r.Method, r.URL.Path, formPath, err)
		return
	}
	_, err = io.WriteString(w, responseText)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		log.Printf("can't write form into response %s %s: %s", r.Method, r.URL.Path, err)
		return
	}
}

// UpdateFormTemplatesHandler handles API calls to update form templates.
func (m *Manager) UpdateFormTemplatesHandler(w http.ResponseWriter, r *http.Request) {
	response, err := m.UpdateFormTemplates(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprint(err), http.StatusInternalServerError)
		return
	}
	jsn, _ := json.Marshal(response)
	_, _ = w.Write(jsn)
}

// UpdateFormTemplates handles searching for and installing the latest version of the form templates.
func (m *Manager) UpdateFormTemplates(ctx context.Context) (UpdateResponse, error) {
	if err := os.MkdirAll(m.config.FormsPath, 0o755); err != nil {
		return UpdateResponse{}, fmt.Errorf("can't write to forms dir [%w]", err)
	}
	log.Printf("Updating form templates; current version is %v", m.getFormsVersion())
	latest, err := m.getLatestFormsInfo(ctx)
	if err != nil {
		return UpdateResponse{}, err
	}
	if !m.isNewerVersion(latest.Version) {
		log.Printf("Latest forms version is %v; nothing to do", latest.Version)
		return UpdateResponse{
			NewestVersion: latest.Version,
			Action:        "none",
		}, nil
	}

	if err = m.downloadAndUnzipForms(ctx, latest.ArchiveURL); err != nil {
		return UpdateResponse{}, err
	}
	log.Printf("Finished forms update to %v", latest.Version)
	// TODO: re-init forms manager
	return UpdateResponse{
		NewestVersion: latest.Version,
		Action:        "update",
	}, nil
}

type formsInfo struct {
	Version    string `json:"version"`
	ArchiveURL string `json:"archive_url"`
}

func (m *Manager) getLatestFormsInfo(ctx context.Context) (*formsInfo, error) {
	resp, err := client.Get(ctx, m.config.UserAgent, formsVersionInfoURL)
	if err != nil || resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("can't fetch winlink forms version page: %w", err)
	}
	defer resp.Body.Close()

	var v formsInfo
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, err
	}
	return &v, nil
}

func (m *Manager) downloadAndUnzipForms(ctx context.Context, downloadLink string) error {
	log.Printf("Updating forms via %v", downloadLink)
	resp, err := client.Get(ctx, m.config.UserAgent, downloadLink)
	if err != nil {
		return fmt.Errorf("can't download update ZIP: %w", err)
	}
	defer resp.Body.Close()
	f, err := os.CreateTemp(os.TempDir(), "pat")
	if err != nil {
		return fmt.Errorf("can't create temp file for download: %w", err)
	}
	defer f.Close()
	defer os.Remove(f.Name())
	if _, err := io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("can't write update ZIP: %w", err)
	}

	if err := unzip(f.Name(), m.config.FormsPath); err != nil {
		return fmt.Errorf("can't unzip forms update: %w", err)
	}
	return nil
}

// RenderForm finds the associated form and returns the filled-in form in HTML given the contents of a form attachment
func (m *Manager) RenderForm(data []byte, inReplyToMsg *fbb.Message, inReplyToPath string) (string, error) {
	type Node struct {
		XMLName xml.Name
		Content []byte `xml:",innerxml"`
		Nodes   []Node `xml:",any"`
	}

	data = trimBom(data)
	if !utf8.Valid(data) {
		log.Println("Warning: unsupported string encoding in form XML, expected UTF-8")
	}

	var n1 Node
	formParams := make(map[string]string)
	formVars := make(map[string]string)

	if err := xml.Unmarshal(data, &n1); err != nil {
		return "", err
	}

	if n1.XMLName.Local != "RMS_Express_Form" {
		return "", errors.New("missing RMS_Express_Form tag in form XML")
	}
	for _, n2 := range n1.Nodes {
		switch n2.XMLName.Local {
		case "form_parameters":
			for _, n3 := range n2.Nodes {
				formParams[n3.XMLName.Local] = string(n3.Content)
			}
		case "variables":
			for _, n3 := range n2.Nodes {
				formVars[n3.XMLName.Local] = string(n3.Content)
			}
		}
	}

	filesMap := formFilesFromPath(m.config.FormsPath)
	switch {
	case inReplyToPath != "":
		replyTemplate := formParams["reply_template"]
		if replyTemplate == "" {
			return "", errors.New("missing reply_template tag in form XML for a reply message")
		}
		if filepath.Ext(replyTemplate) == "" {
			replyTemplate += replyFileExt
		}
		path := filesMap.get(replyTemplate)
		if path == "" {
			return "", fmt.Errorf("reply template not found: %q", replyTemplate)
		}
		template, err := readTemplate(path, filesMap)
		if err != nil {
			return "", fmt.Errorf("failed to read referenced reply template: %w", err)
		}
		submitURL := "/api/form?in-reply-to=" + url.QueryEscape(inReplyToPath) + "&template=" + url.QueryEscape(m.rel(template.Path))
		return m.fillFormTemplate(template.InputFormPath, inReplyToMsg, submitURL, formVars)
	default:
		displayForm := formParams["display_form"]
		if displayForm == "" {
			return "", errors.New("missing display_form tag in form XML")
		}
		if filepath.Ext(displayForm) == "" {
			displayForm += htmlFileExt
		}
		// Viewing a form (initial or reply)
		path := filesMap.get(displayForm)
		if path == "" {
			return "", fmt.Errorf("display from not found: %q", displayForm)
		}
		return m.fillFormTemplate(path, inReplyToMsg, "", formVars)
	}
}

// ComposeTemplate composes a message from a template (templatePath) by prompting the user through stdio.
//
// It combines all data needed for the whole template-based message: subject, body, and attachments.
func (m *Manager) ComposeTemplate(templatePath string, subject string, inReplyToMsg *fbb.Message, lineReader func() string) (Message, error) {
	template, err := readTemplate(templatePath, formFilesFromPath(m.config.FormsPath))
	switch {
	case os.IsNotExist(err) && !filepath.IsAbs(templatePath):
		// Try resolving the path relative to forms directory.
		return m.ComposeTemplate(m.abs(templatePath), subject, inReplyToMsg, lineReader)
	case err != nil:
		return Message{}, err
	}

	formValues := map[string]string{
		"subjectline":     subject,
		"templateversion": m.getFormsVersion(),
	}
	fmt.Printf("Form '%s', version: %s\n", m.rel(template.Path), formValues["templateversion"])
	return messageBuilder{
		Interactive: true,
		LineReader:  lineReader,

		Template:     template,
		FormValues:   formValues,
		FormsMgr:     m,
		InReplyToMsg: inReplyToMsg,
	}.build()
}

func (m *Manager) buildFormFolder() (FormFolder, error) {
	formFolder, err := m.innerRecursiveBuildFormFolder(m.config.FormsPath, formFilesFromPath(m.config.FormsPath))
	formFolder.Version = m.getFormsVersion()
	return formFolder, err
}

func (m *Manager) innerRecursiveBuildFormFolder(rootPath string, filesMap formFilesMap) (FormFolder, error) {
	folder := FormFolder{
		Name:    filepath.Base(rootPath),
		Path:    rootPath,
		Forms:   []Template{},
		Folders: []FormFolder{},
	}
	err := fs.WalkDir(os.DirFS(rootPath), ".", func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case path == ".":
			return nil
		case d.IsDir():
			subfolder, err := m.innerRecursiveBuildFormFolder(filepath.Join(rootPath, path), filesMap)
			if err != nil {
				return err
			}
			folder.Folders = append(folder.Folders, subfolder)
			folder.FormCount += subfolder.FormCount
			return fs.SkipDir
		case !strings.EqualFold(filepath.Ext(d.Name()), txtFileExt):
			return nil
		default:
			template, err := readTemplate(filepath.Join(rootPath, path), filesMap)
			if err != nil {
				debug.Printf("failed to load form file %q: %v", path, err)
				return nil
			}
			// Relative paths for the JSON response
			template.Path = m.rel(template.Path)
			folder.Forms = append(folder.Forms, template)
			folder.FormCount++
			return nil
		}
	})
	sort.Slice(folder.Folders, func(i, j int) bool { return folder.Folders[i].Name < folder.Folders[j].Name })
	sort.Slice(folder.Forms, func(i, j int) bool { return folder.Forms[i].Name < folder.Forms[j].Name })
	return folder, err
}

// abs returns the absolute path of a path relative to m.FormsPath.
//
// It is primarily used to resolve template references from the web gui, which
// are relative to m.config.FormsPath.
func (m *Manager) abs(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(m.config.FormsPath, path)
}

// rel returns a path relative to m.FormsPath.
//
// The web gui uses this variant to reference template files.
func (m *Manager) rel(path string) string {
	if !filepath.IsAbs(path) {
		return path
	}
	rel, err := filepath.Rel(m.config.FormsPath, path)
	if err != nil {
		panic(err)
	}
	return rel
}

const gpsMockAddr = "mock" // Hack for unit testing

// gpsPos returns the current GPS Position
func (m *Manager) gpsPos() (gpsd.Position, error) {
	addr := m.config.GPSd.Addr
	if addr == "" {
		return gpsd.Position{}, errors.New("GPSd: not configured.")
	}
	if addr == gpsMockAddr {
		return gpsd.Position{Lat: 59.41378, Lon: 5.268}, nil
	}
	if !m.config.GPSd.AllowForms {
		return gpsd.Position{}, errors.New("GPSd: allow_forms is disabled. GPS position will not be available in form templates.")
	}

	conn, err := gpsd.Dial(addr)
	if err != nil {
		log.Printf("GPSd daemon: %s", err)
		return gpsd.Position{}, err
	}
	defer conn.Close()

	conn.Watch(true)
	log.Println("Waiting for position from GPSd...")
	// TODO: make the GPSd timeout configurable
	return conn.NextPosTimeout(3 * time.Second)
}

func (m *Manager) fillFormTemplate(templatePath string, inReplyToMsg *fbb.Message, formDestURL string, formVars map[string]string) (string, error) {
	data, err := readFile(templatePath)
	if err != nil {
		return "", err
	}

	// Set the "form server" URL
	data = strings.ReplaceAll(data, "http://{FormServer}:{FormPort}", formDestURL)
	data = strings.ReplaceAll(data, "http://localhost:8001", formDestURL) // Some Canada BC forms are hardcoded to this URL

	// Substitute insertion tags and variables
	data = insertionTagReplacer(m, inReplyToMsg, templatePath, "{", "}")(data)
	data = variableReplacer("{", "}", formVars)(data)

	return data, nil
}

func (m *Manager) getFormsVersion() string {
	str, err := readFile(m.abs("Standard_Forms_Version.dat"))
	if err != nil {
		debug.Printf("failed to open version file: %v", err)
		return "unknown"
	}
	// Drop any whitespace in the string
	// (version 1.1.6.0 was released as `1.1.6\t.0`).
	str = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, str)
	return str
}

func (m *Manager) cleanupOldFormData() {
	m.postedFormData.mu.Lock()
	defer m.postedFormData.mu.Unlock()
	for key, form := range m.postedFormData.m {
		elapsed := time.Since(form.submitted).Hours()
		if elapsed > 24 {
			log.Println("deleting old FormData after", elapsed, "hrs")
			delete(m.postedFormData.m, key)
		}
	}
}

func (m *Manager) isNewerVersion(newestVersion string) bool {
	currentVersion := m.getFormsVersion()
	cv := strings.Split(currentVersion, ".")
	nv := strings.Split(newestVersion, ".")
	for i := 0; i < 4; i++ {
		var cp int64
		if len(cv) > i {
			cp, _ = strconv.ParseInt(cv[i], 10, 16)
		}
		var np int64
		if len(nv) > i {
			np, _ = strconv.ParseInt(nv[i], 10, 16)
		}
		if cp < np {
			return true
		}
	}
	return false
}

type httpClient struct{ http.Client }

func (c httpClient) Get(ctx context.Context, userAgent, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Cache-Control", "no-cache")
	return c.Do(req)
}
