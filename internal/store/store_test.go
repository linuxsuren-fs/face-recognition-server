package store

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "faces.sqlite3"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestEmployeeCRUDAndDuplicate(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	created, err := store.CreateEmployee(ctx, "E001", "张三", "巡检班", true)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == 0 || created.SampleCount != 0 || !created.Enabled {
		t.Fatalf("unexpected created row: %+v", created)
	}

	if _, err := store.CreateEmployee(ctx, "E001", "重复", "", true); !errors.Is(err, ErrDuplicateEmployeeNo) {
		t.Fatalf("duplicate employee_no must be reported, got %v", err)
	}

	employee, err := store.GetEmployee(ctx, created.ID)
	if err != nil || employee.Name != "张三" {
		t.Fatalf("get: %v %+v", err, employee)
	}

	newName := "张三丰"
	disabled := false
	updated, err := store.UpdateEmployee(ctx, created.ID, EmployeeUpdate{Name: &newName, Enabled: &disabled})
	if err != nil || updated.Name != "张三丰" || updated.Enabled {
		t.Fatalf("update: %v %+v", err, updated)
	}

	if _, err := store.GetEmployee(ctx, 99999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing employee must be ErrNotFound, got %v", err)
	}

	list, total, err := store.ListEmployees(ctx, EmployeeFilter{IncludeDisabled: false, Page: 1, PageSize: 10})
	if err != nil || total != 0 || len(list) != 0 {
		t.Fatalf("disabled employee must be filtered: %v %d %+v", err, total, list)
	}
	list, total, err = store.ListEmployees(ctx, EmployeeFilter{IncludeDisabled: true, Page: 1, PageSize: 10})
	if err != nil || total != 1 || len(list) != 1 {
		t.Fatalf("include_disabled must list all: %v %d %+v", err, total, list)
	}

	if err := store.DeleteEmployee(ctx, created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := store.DeleteEmployee(ctx, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete must be ErrNotFound, got %v", err)
	}
}

func TestTemplateLifecycleAndMatchProjection(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	employee, err := store.CreateEmployee(ctx, "E002", "李四", "", true)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	enrollment := Enrollment{
		Embedding:     []float32{1, 0, 0.5},
		ModelVersion:  "sface:test",
		QualityScore:  0.9,
		Image:         []byte{0xFF, 0xD8, 0x00},
		ImageMimeType: "image/jpeg",
	}
	template, err := store.AddTemplate(ctx, employee.ID, enrollment)
	if err != nil {
		t.Fatalf("add template: %v", err)
	}
	if template.EmbeddingDim != 3 || !template.HasImage || template.ModelVersion != "sface:test" {
		t.Fatalf("unexpected template: %+v", template)
	}

	if _, err := store.AddTemplate(ctx, employee.ID, Enrollment{
		Embedding:    []float32{float32(math.NaN())},
		ModelVersion: "sface:test",
		Image:        []byte{1},
	}); !errors.Is(err, ErrInvalidEmbedding) {
		t.Fatalf("NaN embedding must be rejected, got %v", err)
	}
	if _, err := store.AddTemplate(ctx, employee.ID, Enrollment{ModelVersion: "sface:test"}); err == nil {
		t.Fatalf("image-less enrollment without embedding must be rejected")
	}

	samples, err := store.ListTemplates(ctx, employee.ID)
	if err != nil || len(samples) != 1 {
		t.Fatalf("list templates: %v %+v", err, samples)
	}

	fetched, err := store.GetTemplate(ctx, template.ID)
	if err != nil || len(fetched.Embedding) != 12 {
		t.Fatalf("get template: %v %+v", err, fetched)
	}

	matches, err := store.ListMatchTemplates(ctx)
	if err != nil {
		t.Fatalf("match templates: %v", err)
	}
	if len(matches) != 1 || matches[0].Name != "李四" || len(matches[0].Embedding) != 3 {
		t.Fatalf("unexpected match projection: %+v", matches)
	}

	// sample_count must follow templates
	employee, _ = store.GetEmployee(ctx, employee.ID)
	if employee.SampleCount != 1 {
		t.Fatalf("sample_count = %d, want 1", employee.SampleCount)
	}

	if err := store.DeleteTemplate(ctx, employee.ID, template.ID); err != nil {
		t.Fatalf("delete template: %v", err)
	}
	if err := store.DeleteTemplate(ctx, employee.ID, template.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete must be ErrNotFound, got %v", err)
	}

	// deleting the employee must cascade remaining templates
	if _, err := store.AddTemplate(ctx, employee.ID, Enrollment{
		Embedding:    []float32{1},
		ModelVersion: "sface:test",
		Image:        []byte{1},
	}); err != nil {
		t.Fatalf("add second template: %v", err)
	}
	if err := store.DeleteEmployee(ctx, employee.ID); err != nil {
		t.Fatalf("delete employee: %v", err)
	}
	if matches, err := store.ListMatchTemplates(ctx); err != nil || len(matches) != 0 {
		t.Fatalf("templates must cascade with the employee: %v %+v", err, matches)
	}
}

func TestDeletePromotedStrangerEmployee(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	// create a stranger track and promote it to an employee
	embedding := []float32{1, 0, 0}
	track, err := store.CreateStrangerTrack(ctx, embedding, []byte{1}, "image/jpeg", 0.9)
	if err != nil {
		t.Fatalf("creating track: %v", err)
	}
	employee, err := store.PromoteStranger(ctx, track.ID, "STRANGER-1", "未知-1", "sface:test")
	if err != nil {
		t.Fatalf("promoting: %v", err)
	}

	// deleting the promoted employee must not hit the FK constraint from
	// stranger_tracks.promoted_employee_id
	if err := store.DeleteEmployee(ctx, employee.ID); err != nil {
		t.Fatalf("delete promoted stranger employee: %v", err)
	}
}
