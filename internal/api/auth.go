package api

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/LinuxSuRen/face-recognition-server/internal/auth"
	"github.com/LinuxSuRen/face-recognition-server/internal/engine"
	"github.com/LinuxSuRen/face-recognition-server/internal/liveness"
	"github.com/LinuxSuRen/face-recognition-server/internal/recognizer"
	"github.com/LinuxSuRen/face-recognition-server/internal/store"
)

// authAPI hosts login, user management, API tokens and the external
// recognition endpoints.
type authAPI struct {
	deps Deps
	auth *auth.Service
	oidc *auth.OIDCClient
}

func registerAuthRoutes(mux *http.ServeMux, deps Deps) {
	api := &authAPI{
		deps: deps,
		auth: &auth.Service{Store: deps.Store},
		oidc: auth.NewOIDCClient(deps.OIDC),
	}
	mux.HandleFunc("POST /api/v1/auth/login", api.login)
	mux.HandleFunc("POST /api/v1/auth/logout", api.logout)
	mux.HandleFunc("GET /api/v1/auth/session", api.session)
	mux.HandleFunc("POST /api/v1/auth/face-login", api.faceLogin)
	mux.HandleFunc("GET /api/v1/auth/oauth/start", api.oauthStart)
	mux.HandleFunc("GET /api/v1/auth/oauth/callback", api.oauthCallback)

	mux.HandleFunc("GET /api/v1/me/faces", api.listMyFaces)
	mux.HandleFunc("POST /api/v1/me/faces", api.addMyFace)
	mux.HandleFunc("DELETE /api/v1/me/faces/{id}", api.deleteMyFace)

	mux.HandleFunc("GET /api/v1/users", api.listUsers)
	mux.HandleFunc("POST /api/v1/users", api.createUser)
	mux.HandleFunc("PATCH /api/v1/users/{id}", api.updateUser)

	mux.HandleFunc("GET /api/v1/tokens", api.listTokens)
	mux.HandleFunc("POST /api/v1/tokens", api.createToken)
	mux.HandleFunc("DELETE /api/v1/tokens/{id}", api.deleteToken)
	mux.HandleFunc("GET /api/v1/login-events", api.listLoginEvents)
	mux.HandleFunc("GET /api/v1/login-events/{id}/image", api.loginEventImage)
	mux.HandleFunc("GET /api/v1/settings", api.getSettings)
	mux.HandleFunc("PUT /api/v1/settings", api.updateSettings)

	// external face recognition API (bearer tokens, independent of the
	// browser session auth)
	mux.HandleFunc("POST /api/v1/verify", api.external(api.verify, "verify"))
	mux.HandleFunc("POST /api/v1/identify", api.external(api.identify, "identify"))
	mux.HandleFunc("POST /api/v1/enroll", api.external(api.enroll, "enroll"))
}

// ---- browser session auth ----

type loginPayload struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (api *authAPI) login(w http.ResponseWriter, r *http.Request) {
	var body loginPayload
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体必须是包含用户名和密码的 JSON")
		return
	}
	token, user, err := api.auth.Login(r.Context(),
		strings.TrimSpace(body.Username), body.Password)
	if err != nil {
		writeError(w, http.StatusUnauthorized, auth.ErrInvalidCredentials.Error())
		return
	}
	_ = api.deps.Store.CreateLoginEvent(r.Context(),
		user.ID, "password", 0, nil, "", clientIP(r))
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(auth.SessionLifetime.Seconds()),
	})
	writeJSON(w, http.StatusOK, user)
}

func (api *authAPI) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(auth.SessionCookie); err == nil {
		_ = api.auth.Logout(r.Context(), cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: auth.SessionCookie, Value: "", Path: "/", MaxAge: -1,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (api *authAPI) session(w http.ResponseWriter, r *http.Request) {
	payload := map[string]any{
		"authenticated":    false,
		"auth_enabled":     api.deps.AuthEnabled,
		"oidc_configured":  api.deps.OIDC.Configured(),
		"liveness_enabled": api.deps.Store.GetSettingBool(r.Context(), "liveness_enabled", false),
	}
	if user := api.deps.SessionUser(r); user != nil {
		payload["authenticated"] = true
		payload["user"] = user
	}
	writeJSON(w, http.StatusOK, payload)
}

// faceLogin authenticates by face. When the admin enables liveness
// detection, the browser submits several consecutive frames so the server
// can verify independent facial micro-movement (anti photo/replay).
func (api *authAPI) faceLogin(w http.ResponseWriter, r *http.Request) {
	livenessEnabled := api.deps.Store.GetSettingBool(r.Context(), "liveness_enabled", false)
	if api.deps.Engine == nil {
		writeError(w, http.StatusServiceUnavailable, engineUnavailableDetail)
		return
	}

	if livenessEnabled {
		api.faceLoginWithLiveness(w, r)
		return
	}

	upload, status, err := readUpload(r, api.deps.Config.MaxImageBytes, "image")
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	image, err := engine.DecodeImage(upload.Content)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	token, user, similarity, err := api.auth.FaceLogin(r.Context(), image,
		api.deps.Engine, api.deps.Config.KnownThreshold)
	if err != nil {
		api.deps.Logger.Info("[auth][face-login] failed", "reason", err.Error())
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	api.completeFaceLogin(w, r, token, user, similarity, upload.Content, upload.ContentType)
}

// faceLoginWithLiveness reads multiple frames, verifies identity across all
// of them and runs the liveness check before opening the session.
func (api *authAPI) faceLoginWithLiveness(w http.ResponseWriter, r *http.Request) {
	reader, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "请求必须是 multipart/form-data")
		return
	}
	var primaryUpload upload
	var livenessFrames [][]byte
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "malformed multipart request")
			return
		}
		name := part.FormName()
		if part.FileName() == "" {
			part.Close()
			continue
		}
		content, err := io.ReadAll(io.LimitReader(part, api.deps.Config.MaxImageBytes+1))
		mimeType := part.Header.Get("Content-Type")
		part.Close()
		if err != nil || len(content) == 0 {
			continue
		}
		if sniffed := sniffImage(content); sniffed != "" {
			mimeType = sniffed
		}
		if !allowedImageType(mimeType) {
			continue
		}
		if name == "image" {
			primaryUpload = upload{Content: content, ContentType: mimeType}
		} else if name == "liveness" {
			livenessFrames = append(livenessFrames, content)
		}
	}
	if primaryUpload.Content == nil {
		writeError(w, http.StatusBadRequest, "缺少 image 字段")
		return
	}
	if len(livenessFrames) < liveness.MinFrames-1 {
		writeError(w, http.StatusUnprocessableEntity,
			"活体检测已启用：请让浏览器自动采集多帧画面")
		return
	}

	// decode all frames and extract the largest face from each
	var allFaces []engine.ExtractedFace
	primaryImage, err := engine.DecodeImage(primaryUpload.Content)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	primaryFaces, err := api.deps.Engine.Extract(primaryImage)
	if err != nil || len(primaryFaces) == 0 || primaryFaces[0].Embedding == nil {
		writeError(w, http.StatusUnauthorized, auth.ErrNoFace.Error())
		return
	}
	allFaces = append(allFaces, primaryFaces[0])
	for _, frameContent := range livenessFrames {
		frameImage, err := engine.DecodeImage(frameContent)
		if err != nil {
			continue
		}
		frameFaces, err := api.deps.Engine.Extract(frameImage)
		if err != nil || len(frameFaces) == 0 || frameFaces[0].Embedding == nil {
			continue
		}
		allFaces = append(allFaces, frameFaces[0])
	}

	// liveness check
	threshold := api.deps.Store.GetSettingFloat(r.Context(), "liveness_threshold", 0.3)
	livenessResult := liveness.Check(allFaces, threshold)
	if !livenessResult.Live {
		api.deps.Logger.Info("[auth][face-login] liveness failed",
			"score", livenessResult.Score, "reason", livenessResult.Reason)
		writeError(w, http.StatusUnauthorized,
			"活体验证未通过："+livenessResult.Reason)
		return
	}

	// identity check on the primary frame
	token, user, similarity, err := api.auth.FaceLogin(r.Context(), primaryImage,
		api.deps.Engine, api.deps.Config.KnownThreshold)
	if err != nil {
		api.deps.Logger.Info("[auth][face-login] failed", "reason", err.Error())
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	api.deps.Logger.Info("[auth][face-login] liveness passed",
		"score", livenessResult.Score)
	api.completeFaceLogin(w, r, token, user, similarity,
		primaryUpload.Content, primaryUpload.ContentType)
}

func (api *authAPI) completeFaceLogin(w http.ResponseWriter, r *http.Request,
	token string, user *store.User, similarity float64, image []byte, mimeType string) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(auth.SessionLifetime.Seconds()),
	})
	_ = api.deps.Store.CreateLoginEvent(r.Context(),
		user.ID, "face", math.Round(similarity*10000)/10000,
		image, mimeType, clientIP(r))
	api.deps.Logger.Info("[auth][face-login] success",
		"username", user.Username, "similarity", math.Round(similarity*10000)/10000)
	writeJSON(w, http.StatusOK, map[string]any{"user": user})
}

// ---- my bound faces ----

func (api *authAPI) listMyFaces(w http.ResponseWriter, r *http.Request) {
	user := api.deps.SessionUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "请先登录")
		return
	}
	faces, err := api.deps.Store.ListUserFaces(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "加载人脸样本失败")
		return
	}
	if faces == nil {
		faces = []store.UserFace{}
	}
	writeJSON(w, http.StatusOK, PageResponse{
		Data: faces, Page: 1, Total: len(faces), PageSize: MaxPageSize,
	})
}

func (api *authAPI) addMyFace(w http.ResponseWriter, r *http.Request) {
	user := api.deps.SessionUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "请先登录")
		return
	}
	upload, status, err := readUpload(r, api.deps.Config.MaxImageBytes, "image")
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	if api.deps.Engine == nil {
		writeError(w, http.StatusServiceUnavailable, engineUnavailableDetail)
		return
	}
	image, err := engine.DecodeImage(upload.Content)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	faces, err := api.deps.Engine.Extract(image)
	if err != nil {
		api.deps.Logger.Error("[auth][my-face] extraction failed", "error", err)
		writeError(w, http.StatusInternalServerError, "face extraction failed")
		return
	}
	face, err := recognizer.SelectEnrollmentFace(faces)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	created, err := api.deps.Store.AddUserFace(r.Context(), user.ID, store.UserFaceEnrollment{
		Embedding:     face.Embedding,
		ModelVersion:  api.deps.Engine.ModelVersion(),
		QualityScore:  face.Quality.Score,
		Image:         upload.Content,
		ImageMimeType: upload.ContentType,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "保存人脸样本失败")
		return
	}
	api.deps.Logger.Info("[auth][my-face] bound", "username", user.Username,
		"sample_id", created.ID, "quality", face.Quality.Score)
	writeJSON(w, http.StatusCreated, created)
}

func (api *authAPI) deleteMyFace(w http.ResponseWriter, r *http.Request) {
	user := api.deps.SessionUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "请先登录")
		return
	}
	id, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	if err := api.deps.Store.DeleteUserFace(r.Context(), user.ID, id); err != nil {
		writeError(w, http.StatusNotFound, "人脸样本不存在")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (api *authAPI) oauthStart(w http.ResponseWriter, r *http.Request) {
	if !api.deps.OIDC.Configured() {
		writeError(w, http.StatusNotFound, "SSO 登录未配置")
		return
	}
	start, err := api.oidc.StartURL()
	if err != nil {
		api.deps.Logger.Error("[auth][oidc] start failed", "error", err)
		writeError(w, http.StatusBadGateway, "无法连接身份提供方")
		return
	}
	http.Redirect(w, r, start, http.StatusFound)
}

func (api *authAPI) oauthCallback(w http.ResponseWriter, r *http.Request) {
	info, err := api.oidc.UserInfo(r.URL.Query().Get("code"), r.URL.Query().Get("state"))
	if err != nil {
		api.deps.Logger.Error("[auth][oidc] callback failed", "error", err)
		http.Redirect(w, r, "/login?error=oidc_failed", http.StatusFound)
		return
	}
	ctx := r.Context()
	user, err := api.deps.Store.GetUserByOIDCSubject(ctx, info.Subject)
	if errors.Is(err, store.ErrNotFound) {
		user, err = api.deps.Store.CreateUser(ctx, info.Username, info.Name, "", "user", true)
		if err != nil {
			api.deps.Logger.Error("[auth][oidc] provisioning user failed", "error", err)
			http.Redirect(w, r, "/login?error=oidc_provision", http.StatusFound)
			return
		}
		_ = api.deps.Store.LinkOIDC(ctx, user.ID, info.Subject)
	} else if err != nil {
		http.Redirect(w, r, "/login?error=oidc_failed", http.StatusFound)
		return
	}
	if !user.Enabled {
		http.Redirect(w, r, "/login?error=disabled", http.StatusFound)
		return
	}
	token, err := api.auth.CreateSessionForUser(ctx, user.ID)
	if err != nil {
		http.Redirect(w, r, "/login?error=session", http.StatusFound)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(auth.SessionLifetime.Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

// ---- user management (admin) ----

type userPayload struct {
	Username    *string `json:"username"`
	DisplayName *string `json:"display_name"`
	Password    *string `json:"password"`
	Role        *string `json:"role"`
	Enabled     *bool   `json:"enabled"`
}

func (api *authAPI) listUsers(w http.ResponseWriter, r *http.Request) {
	query := parsePageQuery(r)
	users, total, err := api.deps.Store.ListUsers(r.Context(), query.Page, query.PageSize)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "加载用户失败")
		return
	}
	if users == nil {
		users = []store.User{}
	}
	writeJSON(w, http.StatusOK, PageResponse{
		Data: users, Page: query.Page, Total: total, PageSize: query.PageSize,
	})
}

func (api *authAPI) createUser(w http.ResponseWriter, r *http.Request) {
	var body userPayload
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体必须是 JSON 对象")
		return
	}
	username := strings.TrimSpace(deref(body.Username))
	password := deref(body.Password)
	if username == "" || password == "" {
		writeError(w, http.StatusUnprocessableEntity, "用户名和密码不能为空")
		return
	}
	role := derefOr(body.Role, "user")
	if role != "user" && role != "admin" {
		writeError(w, http.StatusUnprocessableEntity, "角色只支持 user 或 admin")
		return
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "密码加密失败")
		return
	}
	user, err := api.deps.Store.CreateUser(r.Context(), username,
		strings.TrimSpace(deref(body.DisplayName)), hash, role, true)
	if errors.Is(err, store.ErrDuplicateUsername) {
		writeError(w, http.StatusConflict, "用户名已存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "创建用户失败")
		return
	}
	writeJSON(w, http.StatusCreated, user)
}

func (api *authAPI) updateUser(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	var body userPayload
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体必须是 JSON 对象")
		return
	}
	update := store.UserUpdate{
		DisplayName: body.DisplayName,
		Role:        body.Role,
		Enabled:     body.Enabled,
	}
	if body.Password != nil {
		if *body.Password == "" {
			writeError(w, http.StatusUnprocessableEntity, "密码不能为空")
			return
		}
		hash, err := auth.HashPassword(*body.Password)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "密码加密失败")
			return
		}
		update.Password = &hash
	}
	user, err := api.deps.Store.UpdateUser(r.Context(), id, update)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "用户不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "更新用户失败")
		return
	}
	writeJSON(w, http.StatusOK, user)
}

// ---- api tokens (admin) ----

func (api *authAPI) listTokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := api.deps.Store.ListAPITokens(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "加载令牌失败")
		return
	}
	if tokens == nil {
		tokens = []store.APIToken{}
	}
	writeJSON(w, http.StatusOK, PageResponse{
		Data: tokens, Page: 1, Total: len(tokens), PageSize: MaxPageSize,
	})
}

func (api *authAPI) createToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体必须是 JSON 对象")
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		writeError(w, http.StatusUnprocessableEntity, "令牌名称不能为空")
		return
	}
	raw, token, err := api.auth.IssueToken(r.Context(), body.Name, body.Scopes)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         token.ID,
		"name":       token.Name,
		"scopes":     token.Scopes,
		"enabled":    token.Enabled,
		"created_at": token.CreatedAt,
		"token":      raw, // shown exactly once
	})
}

func (api *authAPI) deleteToken(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	if err := api.deps.Store.DeleteAPIToken(r.Context(), id); err != nil {
		writeError(w, http.StatusNotFound, "令牌不存在")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- external recognition endpoints ----

func (api *authAPI) external(
	handler func(w http.ResponseWriter, r *http.Request, token *store.APIToken),
	scope string,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "需要 Bearer 令牌")
			return
		}
		token, err := api.auth.AuthenticateBearer(r.Context(),
			strings.TrimPrefix(header, "Bearer "), scope)
		if errors.Is(err, auth.ErrInsufficientScope) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusForbidden, err.Error())
			return
		}
		if err != nil {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "令牌无效或已吊销")
			return
		}
		handler(w, r, token)
	}
}

// verify answers "is this face the given subject?" (1:1, login flows).
func (api *authAPI) verify(w http.ResponseWriter, r *http.Request, _ *store.APIToken) {
	upload, status, err := readUpload(r, api.deps.Config.MaxImageBytes, "image")
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	subject := upload.Fields["subject"]
	if subject == "" {
		writeError(w, http.StatusUnprocessableEntity, "缺少 subject 字段（员工工号）")
		return
	}
	if api.deps.Engine == nil {
		writeError(w, http.StatusServiceUnavailable, engineUnavailableDetail)
		return
	}
	response := map[string]any{"subject": subject, "verified": false, "reason": ""}

	employee, err := api.deps.Store.GetEmployeeByNo(r.Context(), subject)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !employee.Enabled) {
		response["reason"] = "subject_not_found"
		writeJSON(w, http.StatusOK, response)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "加载员工失败")
		return
	}

	image, err := engine.DecodeImage(upload.Content)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	faces, err := api.deps.Engine.Extract(image)
	if err != nil {
		api.deps.Logger.Error("[external][verify] extraction failed", "error", err)
		writeError(w, http.StatusInternalServerError, "face extraction failed")
		return
	}
	if len(faces) == 0 || faces[0].Embedding == nil {
		response["reason"] = "no_usable_face"
		writeJSON(w, http.StatusOK, response)
		return
	}

	templates, err := api.deps.Store.ListTemplates(r.Context(), employee.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "加载模板失败")
		return
	}
	var samples [][]float32
	for _, template := range templates {
		if template.ModelVersion != api.deps.Engine.ModelVersion() || template.EmbeddingDim == 0 {
			continue
		}
		samples = append(samples, decodeTemplateEmbedding(template.Embedding))
	}
	if len(samples) == 0 {
		response["reason"] = "subject_not_enrolled"
		writeJSON(w, http.StatusOK, response)
		return
	}

	best := 0.0
	for _, sample := range samples {
		if similarity := recognizer.Match(faces[0].Embedding, []recognizer.Template{{
			EmployeeID: employee.ID, EmployeeNo: employee.EmployeeNo,
			Name: employee.Name, Embedding: sample,
		}}, recognizer.DefaultThresholds).Similarity; similarity > best {
			best = similarity
		}
	}
	response["similarity"] = math.Round(best*10000) / 10000
	if best >= api.deps.Config.KnownThreshold {
		response["verified"] = true
		response["reason"] = "ok"
		response["employee_id"] = employee.ID
		response["name"] = employee.Name
	} else {
		response["reason"] = "below_threshold"
	}
	writeJSON(w, http.StatusOK, response)
}

// identify answers "who is this face?" (1:N, top matches).
func (api *authAPI) identify(w http.ResponseWriter, r *http.Request, _ *store.APIToken) {
	upload, status, err := readUpload(r, api.deps.Config.MaxImageBytes, "image")
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	if api.deps.Engine == nil {
		writeError(w, http.StatusServiceUnavailable, engineUnavailableDetail)
		return
	}
	image, err := engine.DecodeImage(upload.Content)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	faces, err := api.deps.Engine.Extract(image)
	if err != nil {
		api.deps.Logger.Error("[external][identify] extraction failed", "error", err)
		writeError(w, http.StatusInternalServerError, "face extraction failed")
		return
	}
	response := map[string]any{"face_count": len(faces), "matches": []recognizer.RankedMatch{}}
	if len(faces) == 0 || faces[0].Embedding == nil {
		writeJSON(w, http.StatusOK, response)
		return
	}
	templates, err := api.deps.Store.ListMatchTemplates(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "加载模板失败")
		return
	}
	index := make([]recognizer.Template, 0, len(templates))
	version := api.deps.Engine.ModelVersion()
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
	matches := recognizer.Rank(faces[0].Embedding, index, 3)
	if matches == nil {
		matches = []recognizer.RankedMatch{}
	}
	response["matches"] = matches
	writeJSON(w, http.StatusOK, response)
}

// enroll registers a face for a subject from an external system.
func (api *authAPI) enroll(w http.ResponseWriter, r *http.Request, _ *store.APIToken) {
	upload, status, err := readUpload(r, api.deps.Config.MaxImageBytes, "image")
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	subject := upload.Fields["subject"]
	if subject == "" {
		writeError(w, http.StatusUnprocessableEntity, "缺少 subject 字段（员工工号）")
		return
	}
	name := upload.Fields["name"]
	if name == "" {
		name = subject
	}
	if api.deps.Engine == nil {
		writeError(w, http.StatusServiceUnavailable, engineUnavailableDetail)
		return
	}

	ctx := r.Context()
	employee, err := api.deps.Store.GetEmployeeByNo(ctx, subject)
	if errors.Is(err, store.ErrNotFound) {
		employee, err = api.deps.Store.CreateEmployee(ctx, subject, name, "", true)
		if err != nil {
			if errors.Is(err, store.ErrDuplicateEmployeeNo) {
				writeError(w, http.StatusConflict, "employee_no already exists")
			} else {
				writeError(w, http.StatusInternalServerError, "创建员工失败")
			}
			return
		}
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "加载员工失败")
		return
	}

	image, err := engine.DecodeImage(upload.Content)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	faces, err := api.deps.Engine.Extract(image)
	if err != nil {
		api.deps.Logger.Error("[external][enroll] extraction failed", "error", err)
		writeError(w, http.StatusInternalServerError, "face extraction failed")
		return
	}
	face, err := recognizer.SelectEnrollmentFace(faces)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	existing, err := api.deps.Store.ListTemplates(ctx, employee.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "加载样本失败")
		return
	}
	var samples [][]float32
	for _, template := range existing {
		if template.ModelVersion != api.deps.Engine.ModelVersion() || template.EmbeddingDim == 0 {
			continue
		}
		samples = append(samples, decodeTemplateEmbedding(template.Embedding))
	}
	if !recognizer.ConsistentWith(face.Embedding, samples, recognizer.MinEnrollmentConsistency) {
		writeError(w, http.StatusUnprocessableEntity,
			"face does not match this employee's existing samples")
		return
	}

	template, err := api.deps.Store.AddTemplate(ctx, employee.ID, store.Enrollment{
		Embedding:     face.Embedding,
		ModelVersion:  api.deps.Engine.ModelVersion(),
		QualityScore:  face.Quality.Score,
		FaceBBox:      face.Face.BBoxXYXY,
		Image:         upload.Content,
		ImageMimeType: upload.ContentType,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "保存样本失败")
		return
	}
	api.deps.Logger.Info("[external][enroll] sample stored",
		"subject", subject, "sample_id", template.ID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"subject":       subject,
		"sample_id":     template.ID,
		"quality_score": face.Quality.Score,
	})
}

// ---- login audit ----

func (api *authAPI) listLoginEvents(w http.ResponseWriter, r *http.Request) {
	query := parsePageQuery(r)
	events, total, err := api.deps.Store.ListLoginEvents(r.Context(), query.Page, query.PageSize)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "加载登录记录失败")
		return
	}
	if events == nil {
		events = []store.LoginEvent{}
	}
	for i := range events {
		if events[i].HasImage {
			events[i].ImageMimeType = "/api/v1/login-events/" +
				strconv.FormatInt(events[i].ID, 10) + "/image"
		} else {
			events[i].ImageMimeType = ""
		}
	}
	writeJSON(w, http.StatusOK, PageResponse{
		Data: events, Page: query.Page, Total: total, PageSize: query.PageSize,
	})
}

func (api *authAPI) loginEventImage(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	image, mimeType, err := api.deps.Store.GetLoginEventImage(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "登录照片不存在")
		return
	}
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(image)
}

// ---- system settings (admin) ----

func (api *authAPI) getSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := api.deps.Store.GetSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "加载设置失败")
		return
	}
	if settings == nil {
		settings = store.Settings{}
	}
	// supply defaults so the UI always has complete values
	if _, exists := settings["liveness_enabled"]; !exists {
		settings["liveness_enabled"] = false
	}
	if _, exists := settings["liveness_threshold"]; !exists {
		settings["liveness_threshold"] = 0.3
	}
	if _, exists := settings["stranger_tracking"]; !exists {
		settings["stranger_tracking"] = true
	}
	if _, exists := settings["stranger_promotion_count"]; !exists {
		settings["stranger_promotion_count"] = float64(recognizer.DefaultStrangerPromotionCount)
	}
	writeJSON(w, http.StatusOK, settings)
}

func (api *authAPI) updateSettings(w http.ResponseWriter, r *http.Request) {
	var body store.Settings
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体必须是 JSON 对象")
		return
	}
	// whitelist known settings keys
	allowed := store.Settings{}
	if value, exists := body["liveness_enabled"]; exists {
		if enabled, ok := value.(bool); ok {
			allowed["liveness_enabled"] = enabled
		}
	}
	if value, exists := body["liveness_threshold"]; exists {
		if threshold, ok := value.(float64); ok && threshold >= 0 && threshold <= 1 {
			allowed["liveness_threshold"] = threshold
		}
	}
	if value, exists := body["stranger_tracking"]; exists {
		if enabled, ok := value.(bool); ok {
			allowed["stranger_tracking"] = enabled
		}
	}
	if value, exists := body["stranger_promotion_count"]; exists {
		if count, ok := value.(float64); ok && count >= 2 && count <= 20 {
			allowed["stranger_promotion_count"] = int(count)
		}
	}
	if len(allowed) == 0 {
		writeError(w, http.StatusUnprocessableEntity, "没有可更新的设置项")
		return
	}
	if err := api.deps.Store.UpdateSettings(r.Context(), allowed); err != nil {
		writeError(w, http.StatusInternalServerError, "保存设置失败")
		return
	}
	api.deps.Logger.Info("[settings] updated", "keys", allowed)
	writeJSON(w, http.StatusOK, allowed)
}

// clientIP extracts the best-guess origin address.
func clientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		parts := strings.SplitN(forwarded, ",", 2)
		return strings.TrimSpace(parts[0])
	}
	if real := r.Header.Get("X-Real-IP"); real != "" {
		return real
	}
	host := r.RemoteAddr
	if index := strings.LastIndex(host, ":"); index > 0 {
		host = host[:index]
	}
	return host
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func derefOr(value *string, fallback string) string {
	if value == nil || *value == "" {
		return fallback
	}
	return *value
}
