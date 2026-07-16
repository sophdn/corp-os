package hooks

import "testing"

func TestRegisterAndFireInOrder(t *testing.T) {
	s := NewSurface()
	var order []string
	_ = s.Register(PreTurn, "a", func(*Context) { order = append(order, "a") })
	_ = s.Register(PreTurn, "b", func(*Context) { order = append(order, "b") })
	if s.Count(PreTurn) != 2 {
		t.Fatalf("count = %d, want 2", s.Count(PreTurn))
	}
	s.Fire(PreTurn, &Context{})
	if len(order) != 2 || order[0] != "a" || order[1] != "b" {
		t.Errorf("fire order = %v, want [a b]", order)
	}
}

func TestUnknownKindErrors(t *testing.T) {
	if err := NewSurface().Register(Kind("bogus"), "x", func(*Context) {}); err == nil {
		t.Error("want error for an unknown hook kind")
	}
}

func TestFireIsNonBlocking(t *testing.T) {
	s := NewSurface()
	_ = s.Register(PostTurn, "boom", func(*Context) { panic("kaboom") })
	ran := false
	_ = s.Register(PostTurn, "after", func(*Context) { ran = true })

	ctx := s.Fire(PostTurn, &Context{})
	if !ran {
		t.Error("a hook after a panicking hook should still run")
	}
	if len(ctx.Errors) != 1 || ctx.Errors[0].Hook != "boom" {
		t.Errorf("errors = %+v, want one recorded for boom", ctx.Errors)
	}
}

func TestContextMutationVisible(t *testing.T) {
	s := NewSurface()
	_ = s.Register(PreUserPrompt, "add", func(c *Context) {
		c.SystemPromptAdditions = append(c.SystemPromptAdditions, "extra")
	})
	ctx := s.Fire(PreUserPrompt, &Context{})
	if len(ctx.SystemPromptAdditions) != 1 {
		t.Error("hook mutation to the shared context was lost")
	}
}

func TestDeclaredKinds(t *testing.T) {
	if got := len(NewSurface().DeclaredKinds()); got != 8 {
		t.Errorf("declared kinds = %d, want 8", got)
	}
}
