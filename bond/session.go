package bond

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"sync"

	"github.com/pkg/errors"
	"github.com/upper/db"
	"github.com/upper/db/sqlbuilder"
)

type txWithContext interface {
	WithContext(context.Context) sqlbuilder.Tx
}

type databaseWithContext interface {
	WithContext(context.Context) sqlbuilder.Database
}

type hasContext interface {
	Context() context.Context
}

// Engine represents a bond database engine.
type Engine interface {
	db.Database
	sqlbuilder.SQLBuilder
}

// Session represents
type Session interface {
	Engine

	// Store returns a suitable store for the given table name (string), Model or
	// db.Collection.
	Store(item interface{}) Store

	// Save looks up the given model's store and delegates a Save call to it.
	Save(Model) error

	// Delete looks up the model's store and delegates the Delete call to it.
	Delete(Model) error

	// Context returns the context the session is running in.
	Context() context.Context

	// Transaction runs a transactional operation.
	Transaction(func(Session) error) error

	// TransactionContext runs a transactional operation on the given context.
	TransactionContext(context.Context, func(Session) error) error
}

type session struct {
	Engine

	memoStores map[string]*bondStore
	mu         sync.Mutex
}

// Open connects to a database and returns a Session.
func Open(adapter string, url db.ConnectionURL) (Session, error) {
	conn, err := sqlbuilder.Open(adapter, url)
	if err != nil {
		return nil, err
	}

	sess := New(conn)
	return sess, nil
}

// New returns a new Session.
func New(conn Engine) Session {
	return &session{
		Engine:     conn,
		memoStores: make(map[string]*bondStore),
	}
}

func (s *session) WithContext(ctx context.Context) Session {
	var backendCtx Engine
	switch t := s.Engine.(type) {
	case databaseWithContext:
		backendCtx = t.WithContext(ctx)
	case txWithContext:
		backendCtx = t.WithContext(ctx)
	default:
		panic("Bad session")
	}

	return &session{
		Engine:     backendCtx,
		memoStores: make(map[string]*bondStore),
	}
}

func (s *session) Context() context.Context {
	return s.Engine.(hasContext).Context()
}

// Bind creates a binding between an adapter and a *sql.Tx or a *sql.DB.
func Bind(adapter string, backend sqlbuilder.SQLEngine) (Session, error) {
	var conn Engine

	switch t := backend.(type) {
	case *sql.Tx:
		var err error
		conn, err = sqlbuilder.NewTx(adapter, t)
		if err != nil {
			return nil, err
		}
	case *sql.DB:
		var err error
		conn, err = sqlbuilder.New(adapter, t)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("Unknown backend type: %T", t)
	}

	return &session{
		Engine:     conn,
		memoStores: make(map[string]*bondStore),
	}, nil
}

func (s *session) NewTx(ctx context.Context) (sqlbuilder.Tx, error) {
	return s.Engine.(sqlbuilder.Database).NewTx(ctx)
}

func (s *session) NewSessionTx(ctx context.Context) (Session, error) {
	tx, err := s.NewTx(ctx)
	if err != nil {
		return nil, err
	}
	return &session{
		Engine:     tx,
		memoStores: make(map[string]*bondStore),
	}, nil
}

func (s *session) txCommit() error {
	tx, ok := s.Engine.(sqlbuilder.Tx)
	if !ok {
		return errors.Errorf("bond: session is not a tx")
	}
	defer tx.Close()
	return tx.Commit()
}

func (s *session) txRollback() error {
	tx, ok := s.Engine.(sqlbuilder.Tx)
	if !ok {
		return errors.Errorf("bond: session is not a tx")
	}
	defer tx.Close()
	return tx.Rollback()
}

func (s *session) Transaction(fn func(sess Session) error) error {
	return s.TransactionContext(context.Background(), fn)
}

func (s *session) TransactionContext(ctx context.Context, fn func(sess Session) error) error {
	txFn := func(sess sqlbuilder.Tx) error {
		return fn(&session{
			Engine:     sess,
			memoStores: make(map[string]*bondStore),
		})
	}

	switch t := s.Engine.(type) {
	case sqlbuilder.Database:
		return t.Tx(ctx, txFn)
	case sqlbuilder.Tx:
		defer t.Close()
		err := txFn(t)
		if err != nil {
			if rErr := t.Rollback(); rErr != nil {
				return errors.Wrap(err, rErr.Error())
			}
			return err
		}
		return t.Commit()
	}

	return errors.New("Missing backend, forgot to use bond.New?")
}

func (sess *session) Save(item Model) error {
	if item == nil {
		return ErrExpectingNonNilModel
	}
	return item.Store(sess).Save(item)
}

func (sess *session) Delete(item Model) error {
	if item == nil {
		return ErrExpectingNonNilModel
	}
	return item.Store(sess).Delete(item)
}

func (s *session) Store(item interface{}) Store {
	storeName := s.resolveStoreName(item)
	if storeName == "" {
		return &bondStore{session: s}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if store, ok := s.memoStores[storeName]; ok {
		return store
	}

	store := &bondStore{
		Collection: s.Collection(storeName),
		session:    s,
	}
	s.memoStores[storeName] = store
	return s.memoStores[storeName]
}

func (s *session) resolveStoreName(item interface{}) string {
	// TODO: detect loops

	switch t := item.(type) {
	case string:
		return t
	case func(sess Session) db.Collection:
		return t(s).Name()
	case db.Collection:
		return t.Name()
	case Model:
		return t.Store(s).Name()
	default:
		itemv := reflect.ValueOf(item)
		if itemv.Kind() == reflect.Ptr {
			itemv = reflect.Indirect(itemv)
		}
		item = itemv.Interface()
		if m, ok := item.(Model); ok {
			return m.Store(s).Name()
		}
	}

	return ""
}
