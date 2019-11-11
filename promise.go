package promise

import (
	"context"
	"errors"
	"reflect"
	"sync"
)

const (
	pending = iota
	fulfilled
	rejected
)

// A Promise is a proxy for a value not necessarily known when
// the promise is created. It allows you to associate handlers
// with an asynchronous action's eventual success value or failure reason.
// This lets asynchronous methods return values like synchronous methods:
// instead of immediately returning the final value, the asynchronous method
// returns a promise to supply the value at some point in the future.
type Promise struct {
	mutex sync.Mutex
	cond  *sync.Cond

	// A Promise is in one of these states:
	// Pending - 0. Initial state, neither fulfilled nor rejected.
	// Fulfilled - 1. Operation completed successfully.
	// Rejected - 2. Operation failed.
	state int

	// callbacks
	then []func(data interface{}, err error)

	// the result passed to resolve()
	result interface{}

	// the error passed to reject()
	err error
}

func newPromise() *Promise {
	p := &Promise{state: pending}
	p.cond = sync.NewCond(&p.mutex)
	return p
}

// New instantiates and returns a pointer to the Promise.
func New(executor func(resolve func(interface{}), reject func(error))) *Promise {
	promise := newPromise()

	go func() {
		defer promise.handlePanic()
		executor(promise.resolve, promise.reject)
	}()

	return promise
}

// NewRaw returns a new promise and its resolver and rejector. Use this for promises that will not run promptly.
func NewRaw() (*Promise, func(interface{}), func(error)) {
	promise := newPromise()
	return promise, promise.resolve, promise.reject
}

func (promise *Promise) fulfill(result interface{}, err error) {
	promise.mutex.Lock()

	if promise.state != pending {
		promise.mutex.Unlock()
		return
	}

	for err == nil {
		inner, ok := result.(*Promise)
		if !ok {
			break
		}
		result, err = inner.Await(context.Background())
	}

	if err != nil {
		promise.state, promise.err = rejected, err
	} else {
		promise.state, promise.result = fulfilled, result
	}
	promise.mutex.Unlock()
	promise.cond.Broadcast()

	for _, fn := range promise.then {
		go fn(result, err)
	}
}

func (promise *Promise) resolve(resolution interface{}) {
	promise.fulfill(resolution, nil)
}

func (promise *Promise) reject(err error) {
	promise.fulfill(nil, err)
}

func (promise *Promise) handlePanic() {
	var r = recover()
	if r != nil {
		promise.reject(errors.New(r.(string)))
	}
}

var errorType = reflect.TypeOf((*error)(nil)).Elem()

func deconstructArg(arg reflect.Value, callbackType reflect.Type) ([]reflect.Value, bool) {
	if arg.Kind() != reflect.Slice || arg.Len() < callbackType.NumIn() {
		return nil, false
	}

	args := make([]reflect.Value, callbackType.NumIn())
	for i := 0; i < callbackType.NumIn(); i++ {
		paramType := callbackType.In(i)

		arg := arg.Index(i)
		if arg.Kind() == reflect.Interface {
			arg = arg.Elem()
		}

		if !arg.Type().ConvertibleTo(paramType) {
			return nil, false
		}
		args[i] = arg.Convert(paramType)
	}

	return args, true
}

func checkCallback(callback interface{}) func(interface{}) (interface{}, error) {
	callbackValue := reflect.ValueOf(callback)
	callbackType := callbackValue.Type()
	if callbackValue.Kind() != reflect.Func {
		panic("callback must be a function that accepts one argument")
	}
	switch callbackType.NumOut() {
	case 0, 1:
		// OK
	case 2:
		if !callbackType.Out(1).ConvertibleTo(errorType) {
			panic("callbacks that return two values must return a second value that is convertible to error")
		}
	default:
		panic("callbacks must have 0, 1, or two return values")
	}

	return func(data interface{}) (interface{}, error) {
		arg := reflect.ValueOf(data)
		if arg.Kind() == reflect.Interface {
			arg = arg.Elem()
		}

		args, deconstructed := deconstructArg(arg, callbackType)
		if !deconstructed {
			paramType := callbackType.In(0)
			if paramType.Kind() == reflect.Slice && !arg.Type().ConvertibleTo(paramType) {
				arrayArg := reflect.MakeSlice(paramType, arg.Len(), arg.Len())
				for i := 0; i < arg.Len(); i++ {
					arrayArg.Index(i).Set(arg.Index(i).Convert(paramType.Elem()))
				}
				arg = arrayArg
			}
			args = []reflect.Value{arg.Convert(paramType)}
		}

		result := callbackValue.Call(args)
		switch len(result) {
		case 0:
			return nil, nil
		case 1:
			return result[0].Interface(), nil
		default:
			err, _ := result[1].Convert(errorType).Interface().(error)
			return result[0].Interface(), err
		}
	}
}

// Then appends fulfillment handler to the Promise, and returns a new promise.
func (promise *Promise) Then(fulfillment interface{}) *Promise {
	callback := checkCallback(fulfillment)

	promise.mutex.Lock()

	switch promise.state {
	case pending:
		then := newPromise()
		promise.then = append(promise.then, func(data interface{}, err error) {
			if err != nil {
				then.reject(err)
			} else {
				v, err := callback(data)
				if err != nil {
					then.reject(err)
				} else {
					then.resolve(v)
				}
			}
		})
		promise.mutex.Unlock()
		return then

	case fulfilled:
		promise.mutex.Unlock()

		return New(func(resolve func(interface{}), reject func(error)) {
			v, err := callback(promise.result)
			if err != nil {
				reject(err)
			} else {
				resolve(v)
			}
		})
	}

	promise.mutex.Unlock()
	return promise
}

// Catch appends a rejection handler callback to the Promise, and returns a new promise.
func (promise *Promise) Catch(rejection interface{}) *Promise {
	callback := checkCallback(rejection)

	promise.mutex.Lock()

	switch promise.state {
	case pending:
		then := newPromise()
		promise.then = append(promise.then, func(data interface{}, err error) {
			if err == nil {
				then.resolve(data)
			} else {
				v, err := callback(err)
				if err != nil {
					then.reject(err)
				} else {
					then.resolve(v)
				}
			}
		})
		promise.mutex.Unlock()
		return then

	case rejected:
		promise.mutex.Unlock()

		return New(func(resolve func(interface{}), reject func(error)) {
			v, err := callback(promise.err)
			if err != nil {
				reject(err)
			} else {
				resolve(v)
			}
		})
	}

	promise.mutex.Unlock()
	return promise
}

// Await is a blocking function that waits for all callbacks to be executed.
// Returns value and error.
// Call on an already resolved Promise to get its result and error
func (promise *Promise) Await(ctx context.Context) (interface{}, error) {
	promise.mutex.Lock()
	for promise.state == pending {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		promise.cond.Wait()
	}
	promise.mutex.Unlock()
	return promise.result, promise.err
}

// All waits for all promises to be resolved, or for any to be rejected.
// If the returned promise resolves, it is resolved with an aggregating array of the values
// from the resolved promises in the same order as defined in the iterable of multiple promises.
// If it rejects, it is rejected with the reason from the first promise in the iterable that was rejected.
func All(promises ...*Promise) *Promise {
	if len(promises) == 0 {
		return Resolve([]interface{}{})
	}

	return New(func(resolve func(interface{}), reject func(error)) {
		var wg sync.WaitGroup
		wg.Add(len(promises))

		values := make([]interface{}, len(promises))
		for i, promise := range promises {
			go func(i int, promise *Promise) {
				v, err := promise.Await(context.Background())
				if err != nil {
					reject(err)
				} else {
					values[i] = v
				}
				wg.Done()
			}(i, promise)
		}

		wg.Wait()
		resolve(values)
	})
}

// Race waits until any of the promises is resolved or rejected.
// If the returned promise resolves, it is resolved with the value of the first promise in the iterable
// that resolved. If it rejects, it is rejected with the reason from the first promise that was rejected.
func Race(promises ...*Promise) *Promise {
	if len(promises) == 0 {
		return Resolve(nil)
	}

	return New(func(resolve func(interface{}), reject func(error)) {
		for _, promise := range promises {
			go func(promise *Promise) {
				v, err := promise.Await(context.Background())
				if err != nil {
					reject(err)
				} else {
					resolve(v)
				}
			}(promise)
		}
	})
}

// AllSettled waits until all promises have settled (each may resolve, or reject).
// Returns a promise that resolves after all of the given promises have either resolved or rejected,
// with an array of objects that each describe the outcome of each promise.
func AllSettled(promises ...*Promise) *Promise {
	if len(promises) == 0 {
		return Resolve([]interface{}{})
	}

	return New(func(resolve func(interface{}), _ func(error)) {
		values := make([]interface{}, len(promises))
		for i, p := range promises {
			v, err := p.Await(context.Background())
			if err != nil {
				values[i] = err
			} else {
				values[i] = v
			}
		}
		resolve(values)
	})
}

// Resolve returns a Promise that has been resolved with a given value.
func Resolve(resolution interface{}) *Promise {
	p := newPromise()
	p.state, p.result = fulfilled, resolution
	return p
}

// Reject returns a Promise that has been rejected with a given error.
func Reject(err error) *Promise {
	p := newPromise()
	p.state, p.err = rejected, err
	return p
}
