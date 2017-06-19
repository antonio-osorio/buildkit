package flightcontrol

import (
	"io"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/tonistiigi/buildkit_poc/util/progress"
	"golang.org/x/net/context"
)

// flightcontrol is like singleflight but with support for cancellation and
// nested progress reporting

var errRetry = errors.Errorf("retry")

type Group struct {
	mu sync.Mutex       // protects m
	m  map[string]*call // lazily initialized
}

func (g *Group) Do(ctx context.Context, key string, fn func(ctx context.Context) (interface{}, error)) (v interface{}, err error) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*call)
	}

	if c, ok := g.m[key]; ok { // register 2nd waiter
		g.mu.Unlock()
		v, err := c.wait(ctx)
		if err == errRetry {
			runtime.Gosched()
			return g.Do(ctx, key, fn)
		}
		return v, err
	}

	c := newCall(fn)
	g.m[key] = c
	go func() {
		// cleanup after a caller has returned
		<-c.ready
		g.mu.Lock()
		delete(g.m, key)
		g.mu.Unlock()
	}()
	g.mu.Unlock()
	return c.wait(ctx)
}

type call struct {
	mu     sync.Mutex
	result interface{}
	err    error
	ready  chan struct{}

	ctx  *sharedContext
	ctxs []context.Context
	fn   func(ctx context.Context) (interface{}, error)
	once sync.Once

	closeProgressWriter func()
	progressState       *progressState
}

func newCall(fn func(ctx context.Context) (interface{}, error)) *call {
	c := &call{
		fn:            fn,
		ready:         make(chan struct{}),
		progressState: newProgressState(),
	}
	ctx := newContext(c) // newSharedContext
	pr, _, closeProgressWriter := progress.NewContext(ctx)

	c.ctx = ctx
	c.closeProgressWriter = closeProgressWriter

	go c.progressState.run(pr) // TODO: remove this, wrap writer instead

	return c
}

func (c *call) run() {
	defer c.closeProgressWriter()
	v, err := c.fn(c.ctx)
	c.mu.Lock()
	c.result = v
	c.err = err
	c.mu.Unlock()
	close(c.ready)
}

func (c *call) wait(ctx context.Context) (v interface{}, err error) {
	c.mu.Lock()
	// detect case where caller has just returned, let it clean up before
	select {
	case <-c.ready: // could return if no error
		c.mu.Unlock()
		return nil, errRetry
	default:
	}
	c.append(ctx)
	c.mu.Unlock()

	go c.once.Do(c.run)

	select {
	case <-ctx.Done():
		select {
		case <-c.ctx.Done():
			// if this cancelled the last context, then wait for function to shut down
			// and don't accept any more callers
			<-c.ready
			return c.result, c.err
		default:
			return nil, ctx.Err()
		}
	case <-c.ready:
		return c.result, c.err // shared not implemented yet
	}
}

func (c *call) append(ctx context.Context) {
	pw, ok, ctx := progress.FromContext(ctx)
	if ok {
		c.progressState.add(pw)
	}
	c.ctxs = append(c.ctxs, ctx)
	go func() {
		select {
		case <-c.ctx.done:
		case <-ctx.Done():
			c.mu.Lock()
			c.ctx.signal()
			c.mu.Unlock()
		}
	}()
}

func (c *call) Deadline() (deadline time.Time, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ctx := range c.ctxs {
		select {
		case <-ctx.Done():
		default:
			dl, ok := ctx.Deadline()
			if ok {
				return dl, ok
			}
		}
	}
	return time.Time{}, false
}

func (c *call) Done() <-chan struct{} {
	c.mu.Lock()
	c.ctx.signal()
	c.mu.Unlock()
	return c.ctx.done
}

func (c *call) Err() error {
	select {
	case <-c.ctx.Done():
		return c.ctx.err
	default:
		return nil
	}
}

func (c *call) Value(key interface{}) interface{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ctx := range append([]context.Context{}, c.ctxs...) {
		select {
		case <-ctx.Done():
		default:
			if v := ctx.Value(key); v != nil {
				return v
			}
		}
	}
	return nil
}

type sharedContext struct {
	*call
	done chan struct{}
	err  error
}

func newContext(c *call) *sharedContext {
	return &sharedContext{call: c, done: make(chan struct{})}
}

// call with lock
func (c *sharedContext) signal() {
	select {
	case <-c.done:
	default:
		var err error
		for _, ctx := range c.ctxs {
			select {
			case <-ctx.Done():
				err = ctx.Err()
			default:
				return
			}
		}
		c.err = err
		close(c.done)
	}
}

type rawProgressWriter interface {
	WriteRawProgress(*progress.Progress) error
	Close() error
}

type progressState struct {
	mu      sync.Mutex
	items   map[string]*progress.Progress
	writers []rawProgressWriter
	done    bool
}

func newProgressState() *progressState {
	return &progressState{
		items: make(map[string]*progress.Progress),
	}
}

func (ps *progressState) run(pr progress.Reader) {
	for {
		p, err := pr.Read(context.TODO())
		if err != nil {
			if err == io.EOF {
				ps.mu.Lock()
				ps.done = true
				ps.mu.Unlock()
				for _, w := range ps.writers {
					w.Close()
				}
			}
			return
		}
		ps.mu.Lock()
		for _, p := range p {
			for _, w := range ps.writers {
				w.WriteRawProgress(p)
			}
			ps.items[p.ID] = p
		}
		ps.mu.Unlock()
	}
}

func (ps *progressState) add(pw progress.Writer) {
	rw, ok := pw.(rawProgressWriter)
	if !ok {
		return
	}
	ps.mu.Lock()
	plist := make([]*progress.Progress, 0, len(ps.items))
	for _, p := range ps.items {
		plist = append(plist, p)
	}
	sort.Slice(plist, func(i, j int) bool {
		return plist[i].Timestamp.Before(plist[j].Timestamp)
	})
	for _, p := range plist {
		rw.WriteRawProgress(p)
	}
	if ps.done {
		rw.Close()
	} else {
		ps.writers = append(ps.writers, rw)
	}
	ps.mu.Unlock()
}
