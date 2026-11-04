package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/LinuxSuRen/face-recognition-server/internal/config"
	"github.com/LinuxSuRen/face-recognition-server/internal/engine"
	"github.com/LinuxSuRen/face-recognition-server/internal/recognizer"
	"github.com/LinuxSuRen/face-recognition-server/internal/store"
)

// facesAPI implements the /api/v1/faces surface.
type facesAPI struct {
	config config.Config
	logger *slog.Logger
	store  *store.Store
	engine engine.Engine
}

// registerFacesRoutes mounts every faces endpoint on the mux.
func registerFacesRoutes(mux *http.ServeMux, api *facesAPI) {
	mux.HandleFunc("POST /api/v1/faces/employees", api.createEmployee)
	mux.HandleFunc("GET /api/v1/faces/employees", api.listEmployees)
	mux.HandleFunc("GET /api/v1/faces/employees/{id}", api.getEmployee)
	mux.HandleFunc("PATCH /api/v1/faces/employees/{id}", api.updateEmployee)
	mux.HandleFunc("DELETE /api/v1/faces/employees/{id}", api.deleteEmployee)
	mux.HandleFunc("POST /api/v1/faces/employees/{id}/samples", api.addSample)
	mux.HandleFunc("GET /api/v1/faces/employees/{id}/samples", api.listSamples)
	mux.HandleFunc("GET /api/v1/faces/employees/{id}/samples/{sample_id}/image", api.sampleImage)
	mux.HandleFunc("DELETE /api/v1/faces/employees/{id}/samples/{sample_id}", api.deleteSample)
	mux.HandleFunc("POST /api/v1/faces/recognize", api.recognize)
}

// ---- employees ----

type employeePayload struct {
	EmployeeNo *string `json:"employee_no"`
	Name       *string `json:"name"`
	Department *string `json:"department"`
	Enabled    *bool   `json:"enabled"`
}

func (api *facesAPI) createEmployee(w http.ResponseWriter, r *http.Request) {
	body, status, err := strictBody(r)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	employeeNo := cleanText(body.EmployeeNo)
	name := cleanText(body.Name)
	if employeeNo == "" || name == "" {
		writeError(w, 422, "employee_no and name must not be blank")
		return
	}
	if len(employeeNo) > 100 || len(name) > 100 {
		writeError(w, 422, "employee_no and name must be at most 100 characters")
		return
	}
	department := cleanText(body.Department)
	if len(department) > 200 {
		writeError(w, 422, "department must be at most 200 characters")
		return
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	employee, err := api.store.CreateEmployee(r.Context(), employeeNo, name, department, enabled)
	if err != nil {
		api.writeStoreError(w, err, "employee not found")
		return
	}
	writeJSON(w, http.StatusCreated, employee)
}

func (api *facesAPI) listEmployees(w http.ResponseWriter, r *http.Request) {
	query := parsePageQuery(r)
	includeDisabled := r.URL.Query().Get("include_disabled") != "false"
	employees, total, err := api.store.ListEmployees(r.Context(), store.EmployeeFilter{
		IncludeDisabled: includeDisabled,
		Page:            query.Page,
		PageSize:        query.PageSize,
	})
	if err != nil {
		api.writeStoreError(w, err, "employee not found")
		return
	}
	writeJSON(w, http.StatusOK, PageResponse{
		Data: employeesOrEmpty(employees), Page: query.Page, Total: total, PageSize: query.PageSize,
	})
}

func (api *facesAPI) getEmployee(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	employee, err := api.store.GetEmployee(r.Context(), id)
	if err != nil {
		api.writeStoreError(w, err, "employee not found")
		return
	}
	samples, err := api.store.ListTemplates(r.Context(), id)
	if err != nil {
		api.writeStoreError(w, err, "employee not found")
		return
	}
	payload := struct {
		store.Employee
		Samples []map[string]any `json:"samples"`
	}{Employee: *employee, Samples: sampleSummaries(id, samples)}
	writeJSON(w, http.StatusOK, payload)
}

func (api *facesAPI) updateEmployee(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	body, status, err := strictBody(r)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	update := store.EmployeeUpdate{DepartmentSet: body.Department != nil}
	if body.EmployeeNo != nil {
		value := cleanText(body.EmployeeNo)
		if value == "" {
			writeError(w, 422, "employee_no must not be blank")
			return
		}
		update.EmployeeNo = &value
	}
	if body.Name != nil {
		value := cleanText(body.Name)
		if value == "" {
			writeError(w, 422, "name must not be blank")
			return
		}
		update.Name = &value
	}
	if body.Department != nil {
		value := cleanText(body.Department)
		update.Department = &value
	}
	update.Enabled = body.Enabled
	employee, err := api.store.UpdateEmployee(r.Context(), id, update)
	if err != nil {
		api.writeStoreError(w, err, "updating employee")
		return
	}
	writeJSON(w, http.StatusOK, employee)
}

func (api *facesAPI) deleteEmployee(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	if err := api.store.DeleteEmployee(r.Context(), id); err != nil {
		api.writeStoreError(w, err, "deleting employee")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- samples ----

func (api *facesAPI) addSample(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	if _, err := api.store.GetEmployee(r.Context(), id); err != nil {
		api.writeStoreError(w, err, "employee not found")
		return
	}
	upload, status, err := readUpload(r, api.config.MaxImageBytes, "file")
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	if api.engine == nil {
		writeError(w, http.StatusServiceUnavailable, engineUnavailableDetail)
		return
	}
	image, err := engine.DecodeImage(upload.Content)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	faces, err := api.engine.Extract(image)
	if err != nil {
		api.logger.Error("[faces][enroll] extraction failed", "error", err)
		writeError(w, http.StatusInternalServerError, "face extraction failed")
		return
	}
	face, err := recognizer.SelectEnrollmentFace(faces)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	// reject photos that clearly disagree with this employee's samples
	existing, err := api.store.ListTemplates(r.Context(), id)
	if err != nil {
		api.writeStoreError(w, err, "employee not found")
		return
	}
	var samples [][]float32
	for _, template := range existing {
		if template.ModelVersion != api.engine.ModelVersion() || template.EmbeddingDim == 0 {
			continue
		}
		samples = append(samples, decodeTemplateEmbedding(template.Embedding))
	}
	if !recognizer.ConsistentWith(face.Embedding, samples, recognizer.MinEnrollmentConsistency) {
		writeError(w, http.StatusUnprocessableEntity,
			"face does not match this employee's existing samples")
		return
	}

	template, err := api.store.AddTemplate(r.Context(), id, store.Enrollment{
		Embedding:     face.Embedding,
		ModelVersion:  api.engine.ModelVersion(),
		QualityScore:  face.Quality.Score,
		FaceBBox:      face.Face.BBoxXYXY,
		Image:         upload.Content,
		ImageMimeType: upload.ContentType,
	})
	if err != nil {
		api.writeStoreError(w, err, "employee not found")
		return
	}
	api.logger.Info("[faces][enroll] sample stored",
		"employee_id", id, "sample_id", template.ID,
		"quality", face.Quality.Score)
	writeJSON(w, http.StatusCreated, struct {
		store.Template
		FaceBBox [4]float32 `json:"face_bbox"`
	}{Template: publicTemplate(*template), FaceBBox: face.Face.BBoxXYXY})
}

func (api *facesAPI) listSamples(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	if _, err := api.store.GetEmployee(r.Context(), id); err != nil {
		api.writeStoreError(w, err, "employee not found")
		return
	}
	samples, err := api.store.ListTemplates(r.Context(), id)
	if err != nil {
		api.writeStoreError(w, err, "employee not found")
		return
	}
	query := parsePageQueryDefault(r, 100)
	items := sampleSummaries(id, samples)
	writeJSON(w, http.StatusOK, PageResponse{
		Data: items, Page: query.Page, Total: len(samples), PageSize: query.PageSize,
	})
}

func (api *facesAPI) sampleImage(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	sampleID, ok := parseID(w, r, "sample_id")
	if !ok {
		return
	}
	template, err := api.store.GetTemplate(r.Context(), sampleID)
	if err != nil || template.EmployeeID != id || len(template.Image) == 0 {
		writeError(w, http.StatusNotFound, "face sample image not found")
		return
	}
	contentType := template.ImageMimeType
	if contentType == "" {
		contentType = "image/jpeg"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(template.Image); err != nil {
		api.logger.Warn("[faces] writing sample image failed", "error", err)
	}
}

func (api *facesAPI) deleteSample(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	sampleID, ok := parseID(w, r, "sample_id")
	if !ok {
		return
	}
	if err := api.store.DeleteTemplate(r.Context(), id, sampleID); err != nil {
		api.writeStoreError(w, err, "face sample not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- recognition ----

type faceQualityPayload struct {
	Accepted     bool     `json:"accepted"`
	Score        float64  `json:"score"`
	Reasons      []string `json:"reasons"`
	BlurVariance float64  `json:"blur_variance"`
	FaceSize     int      `json:"face_size"`
}

type facePayload struct {
	ID         int        `json:"id"`
	ClassName  string     `json:"class_name"`
	Confidence float64    `json:"confidence"`
	BBoxXYXY   [4]float32 `json:"bbox_xyxy"`
	recognizer.Result
	Quality faceQualityPayload `json:"quality"`
}

func (api *facesAPI) recognize(w http.ResponseWriter, r *http.Request) {
	upload, status, err := readUpload(r, api.config.MaxImageBytes, "image")
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	if api.engine == nil {
		writeError(w, http.StatusServiceUnavailable, engineUnavailableDetail)
		return
	}
	image, err := engine.DecodeImage(upload.Content)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	payload, err := api.analyze(r.Context(), image)
	if err != nil {
		api.logger.Error("[faces][recognize] analysis failed", "error", err)
		writeError(w, http.StatusInternalServerError, "face analysis failed")
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

// analyze runs extraction plus matching for one image; it backs both the
// recognize endpoint and the RTSP live sessions.
func (api *facesAPI) analyze(ctx context.Context, img *engine.Image) (map[string]any, error) {
	if api.engine == nil {
		return nil, errors.New(engineUnavailableDetail)
	}
	faces, err := api.engine.Extract(img)
	if err != nil {
		return nil, err
	}
	templates, err := api.store.ListMatchTemplates(ctx)
	if err != nil {
		return nil, err
	}
	version := api.engine.ModelVersion()
	index := make([]recognizer.Template, 0, len(templates))
	for _, template := range templates {
		if template.ModelVersion != version {
			continue
		}
		index = append(index, recognizer.Template{
			EmployeeID:   template.EmployeeID,
			EmployeeNo:   template.EmployeeNo,
			Name:         template.Name,
			Embedding:    template.Embedding,
			ModelVersion: template.ModelVersion,
		})
	}
	thresholds := recognizer.Thresholds{
		Known:   api.config.KnownThreshold,
		Unknown: api.config.UnknownThreshold,
	}
	payloads := make([]facePayload, 0, len(faces))
	summary := map[string]int{"count": 0, "known": 0, "unknown": 0, "unusable": 0}
	for i, face := range faces {
		payload := facePayload{
			ID:         i + 1,
			ClassName:  "face",
			Confidence: face.Quality.DetectorConfidence,
			BBoxXYXY:   roundBox(face.Face.BBoxXYXY),
			Quality: faceQualityPayload{
				Accepted:     face.Quality.Accepted,
				Score:        face.Quality.Score,
				Reasons:      face.Quality.Reasons,
				BlurVariance: face.Quality.BlurVariance,
				FaceSize:     face.Quality.FaceSize,
			},
		}
		if face.Embedding == nil {
			payload.Result = recognizer.Result{Status: "unusable", Reason: "poor_quality"}
		} else {
			payload.Result = recognizer.Match(face.Embedding, index, thresholds)
		}
		if payload.Quality.Reasons == nil {
			payload.Quality.Reasons = []string{}
		}
		summary["count"]++
		summary[payload.Result.Status]++
		payloads = append(payloads, payload)
	}

	// auto-track strangers: unknown faces that keep appearing get their own
	// library entry after enough sightings
	api.trackStrangers(ctx, img, faces, payloads)

	return map[string]any{"faces": payloads, "summary": summary}, nil
}

// trackStrangers feeds every unknown face into the stranger tracker.
func (api *facesAPI) trackStrangers(ctx context.Context,
	img *engine.Image, faces []engine.ExtractedFace, payloads []facePayload) {
	setting := api.store.GetSettingBool(ctx, "stranger_tracking", true)
	if !setting {
		return
	}
	promotionCount := int(api.store.GetSettingFloat(ctx, "stranger_promotion_count",
		float64(recognizer.DefaultStrangerPromotionCount)))

	tracker := &recognizer.StrangerTracker{
		Store:          api.store,
		KnowmThreshold: api.config.KnownThreshold,
	}
	for i, face := range faces {
		if i >= len(payloads) || face.Embedding == nil {
			continue
		}
		if payloads[i].Result.Status != "unknown" {
			continue
		}
		// encode the face region for the track record
		box := face.Face.BBoxXYXY
		x1 := clampInt(int(box[0]), 0, img.Width)
		y1 := clampInt(int(box[1]), 0, img.Height)
		x2 := clampInt(int(box[2]), 0, img.Width)
		y2 := clampInt(int(box[3]), 0, img.Height)
		if x2 <= x1 || y2 <= y1 {
			continue
		}
		crop := img.Crop(x1, y1, x2, y2)
		jpeg, err := engine.EncodeJPEG(crop, 80)
		if err != nil {
			continue
		}
		track, employee, err := tracker.Track(ctx,
			face.Embedding, jpeg, "image/jpeg",
			face.Quality.Score, promotionCount)
		if err != nil {
			api.logger.Warn("[faces][stranger] tracking failed",
				"error", err, "track_id", track.ID)
			continue
		}
		if employee != nil {
			// promoted: update the face payload to reflect the new match
			api.logger.Info("[faces][stranger] promoted",
				"employee_no", employee.EmployeeNo, "name", employee.Name,
				"appearances", track.AppearanceCount)
			payloads[i].Result = recognizer.Result{
				Status:              "known",
				EmployeeID:          employee.ID,
				EmployeeNo:          employee.EmployeeNo,
				Name:                employee.Name,
				Similarity:          1.0,
				CandidateEmployeeID: employee.ID,
				CandidateEmployeeNo: employee.EmployeeNo,
				CandidateName:       employee.Name,
			}
		}
	}
}

// ---- helpers ----

const engineUnavailableDetail = "face recognition engine is not loaded"

func (api *facesAPI) writeStoreError(w http.ResponseWriter, err error, notFoundDetail string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, notFoundDetail)
	case errors.Is(err, store.ErrDuplicateEmployeeNo):
		writeError(w, http.StatusConflict, "employee_no already exists")
	default:
		api.logger.Error("[faces] store failure", "error", err)
		writeError(w, http.StatusInternalServerError, "storage operation failed")
	}
}

// strictBody decodes a JSON object rejecting unknown fields, blank strings
// and oversized bodies.
func strictBody(r *http.Request) (employeePayload, int, error) {
	var body employeePayload
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		return body, http.StatusBadRequest, errors.New("request body must be a valid employee JSON object")
	}
	return body, 0, nil
}

func cleanText(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func parseID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid id in path")
		return 0, false
	}
	return id, true
}

type upload struct {
	Content     []byte
	ContentType string
	// Fields carries the non-file form fields sent alongside the file.
	Fields map[string]string
}

// readUpload streams the multipart body, enforcing the size cap and
// sniffing the real content type (declared types are unreliable in the
// wild, the actual bytes are not). Non-file fields are collected so
// handlers can read them after the stream has been consumed.
func readUpload(r *http.Request, maxBytes int64, field string) (upload, int, error) {
	reader, err := r.MultipartReader()
	if err != nil {
		return upload{}, http.StatusBadRequest, errors.New("request must be multipart/form-data with a file field")
	}
	result := upload{Fields: map[string]string{}}
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return upload{}, http.StatusBadRequest, errors.New("malformed multipart request")
		}
		if part.FormName() != field || part.FileName() == "" {
			if part.FileName() == "" {
				value, readErr := io.ReadAll(io.LimitReader(part, 1<<16))
				if readErr == nil {
					result.Fields[part.FormName()] = strings.TrimSpace(string(value))
				}
			}
			part.Close()
			continue
		}
		content, err := io.ReadAll(io.LimitReader(part, maxBytes+1))
		part.Close()
		if err != nil {
			return upload{}, http.StatusBadRequest, errors.New("failed to read the uploaded file")
		}
		if len(content) == 0 {
			return upload{}, http.StatusBadRequest, errors.New("image is empty")
		}
		if int64(len(content)) > maxBytes {
			return upload{}, http.StatusRequestEntityTooLarge,
				errors.New("image exceeds the " + strconv.FormatInt(maxBytes>>20, 10) + " MiB limit")
		}
		contentType := part.Header.Get("Content-Type")
		if sniffed := sniffImage(content); sniffed != "" {
			contentType = sniffed
		}
		if !allowedImageType(contentType) {
			return upload{}, http.StatusBadRequest, errors.New("file must be a JPEG, PNG, WebP or GIF image")
		}
		result.Content = content
		result.ContentType = contentType
	}
	if result.Content == nil {
		return upload{}, http.StatusBadRequest, errors.New("missing file field " + field)
	}
	return result, 0, nil
}

func sniffImage(content []byte) string {
	switch {
	case len(content) >= 3 && content[0] == 0xFF && content[1] == 0xD8:
		return "image/jpeg"
	case len(content) >= 8 && string(content[1:4]) == "PNG":
		return "image/png"
	case len(content) >= 12 && string(content[0:4]) == "RIFF" && string(content[8:12]) == "WEBP":
		return "image/webp"
	case len(content) >= 6 && (string(content[0:3]) == "GIF"):
		return "image/gif"
	}
	return ""
}

func allowedImageType(contentType string) bool {
	switch strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0])) {
	case "image/jpeg", "image/png", "image/webp", "image/gif":
		return true
	}
	return false
}

func sampleSummaries(employeeID int64, samples []store.Template) []map[string]any {
	items := make([]map[string]any, 0, len(samples))
	for _, template := range samples {
		item := map[string]any{
			"id":              template.ID,
			"employee_id":     template.EmployeeID,
			"embedding_dim":   template.EmbeddingDim,
			"has_image":       template.HasImage,
			"image_mime_type": template.ImageMimeType,
			"model_version":   template.ModelVersion,
			"quality_score":   template.QualityScore,
			"created_at":      template.CreatedAt,
		}
		if template.HasImage {
			item["image_url"] = "/api/v1/faces/employees/" +
				strconv.FormatInt(employeeID, 10) + "/samples/" +
				strconv.FormatInt(template.ID, 10) + "/image"
		}
		items = append(items, item)
	}
	return items
}

// publicTemplate hides the raw blobs from JSON responses.
func publicTemplate(template store.Template) store.Template {
	template.Embedding = nil
	template.Image = nil
	return template
}

func employeesOrEmpty(employees []store.Employee) []store.Employee {
	if employees == nil {
		return []store.Employee{}
	}
	return employees
}

func decodeTemplateEmbedding(blob []byte) []float32 {
	values := make([]float32, len(blob)/4)
	for i := range values {
		bits := uint32(blob[i*4]) | uint32(blob[i*4+1])<<8 |
			uint32(blob[i*4+2])<<16 | uint32(blob[i*4+3])<<24
		values[i] = math.Float32frombits(bits)
	}
	return values
}

func roundBox(box [4]float32) [4]float32 {
	return [4]float32{
		float32(float64(int64(float64(box[0])*100+0.5)) / 100),
		float32(float64(int64(float64(box[1])*100+0.5)) / 100),
		float32(float64(int64(float64(box[2])*100+0.5)) / 100),
		float32(float64(int64(float64(box[3])*100+0.5)) / 100),
	}
}

func clampInt(v, low, high int) int {
	if v < low { return low }
	if v > high { return high }
	return v
}
