package todo_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Todo represents a todo item.
type Todo struct {
	ID      int       `json:"id"`
	Title   string    `json:"title"`
	IsDone  bool      `json:"is_done"`
	Created time.Time `json:"created"`
}

// Store represents an in-memory store for todos.
type Store struct {
	mu     sync.Mutex
	todos  map[int]Todo
	nextID int
}

// NewStore creates a new store.
func NewStore() *Store {
	return &Store{
		todos:  make(map[int]Todo),
		nextID: 1,
	}
}

// Add adds a new todo item to the store.
func (s *Store) Add(title string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	newTodo := Todo{
		ID:      s.nextID,
		Title:   title,
		IsDone:  false,
		Created: time.Now(),
	}
	s.todos[s.nextID] = newTodo
	s.nextID++
	return newTodo.ID, nil
}

// List lists all todos in the store.
func (s *Store) List() []Todo {
	s.mu.Lock()
	defer s.mu.Unlock()

	var list []Todo
	for _, t := range s.todos {
		list = append(list, t)
	}
	return list
}

// Complete marks a todo as completed.
func (s *Store) Complete(id int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.todos[id]
	if !ok {
		return fmt.Errorf("todo not found")
	}
	t.IsDone = true
	s.todos[id] = t
	return nil
}

func TestStore(t *testing.T) {
	s := NewStore()
	id, err := s.Add("Task 1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(id); err != nil {
		t.Fatal(err)
	}
	todos := s.List()
	if len(todos) != 1 || !todos[0].IsDone {
		t.Errorf("Expected done todo, got %+v", todos)
	}
}

func TestHandlers(t *testing.T) {
	store := NewStore()
	mux := http.NewServeMux()
	mux.HandleFunc("/todos", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(store.List())
		} else if r.Method == http.MethodPost {
			var d struct{ Title string }
			json.NewDecoder(r.Body).Decode(&d)
			id, _ := store.Add(d.Title)
			json.NewEncoder(w).Encode(map[string]int{"id": id})
		}
	})

	// POST /todos
	req := httptest.NewRequest("POST", "/todos", strings.NewReader(`{"title": "Test"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("Expected 200, got %d", w.Code)
	}

	// GET /todos
	req = httptest.NewRequest("GET", "/todos", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("Expected 200, got %d", w.Code)
	}
}
