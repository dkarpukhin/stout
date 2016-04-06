package isolate

import (
	"fmt"

	"golang.org/x/net/context"
)

const (
	spool       = 0
	spoolCancel = 0

	replySpoolOk    = 0
	replySpoolError = 1

	spawn     = 1
	spawnKill = 0

	replySpawnWrite = 0
	replySpawnError = 1
	replySpawnClose = 2
)

type initialDispatch struct {
	ctx context.Context
}

func newInitialDispatch(ctx context.Context) Dispatcher {
	return &initialDispatch{ctx: ctx}
}

func (d *initialDispatch) Handle(msg *message) (Dispatcher, error) {
	switch msg.Number {
	case spool:
		return d.onSpool(msg)
	case spawn:
		return d.onSpawn(msg)
	default:
		return nil, fmt.Errorf("unknown transition id: %d", msg.Number)
	}
}

func (d *initialDispatch) onSpool(msg *message) (Dispatcher, error) {
	var (
		opts Profile
		name string
	)

	GetLogger(d.ctx).Infof("initialDispatch.Handle.Spool().Args. Profile `%+v`, appname `%s`",
		msg.Args[0], msg.Args[1])
	if err := unpackArgs(d.ctx, msg.Args, &opts, &name); err != nil {
		reply(d.ctx, replySpoolError, [2]int{42, 42}, fmt.Sprintf("unbale to unpack args: %v", err))
		return nil, err
	}

	isolateType := opts.Type()
	if isolateType == "" {
		err := fmt.Errorf("the profile does not have `type` option: %v", opts)
		reply(d.ctx, replySpoolError, [2]int{42, 42}, err.Error())
		return nil, err
	}

	box, ok := getBoxes(d.ctx)[isolateType]
	if !ok {
		err := fmt.Errorf("isolate type %s is not available", isolateType)
		reply(d.ctx, replySpoolError, [2]int{42, 42}, err.Error())
		return nil, err
	}

	ctx, cancel := context.WithCancel(d.ctx)

	go func() {
		if err := box.Spool(ctx, name, opts); err != nil {
			reply(ctx, replySpoolError, [2]int{42, 42}, err.Error())
			return
		}
		// NOTE: make sure that nil is packed as []interface{}
		reply(ctx, replySpoolOk, nil)
	}()

	return newSpoolCancelationDispatch(ctx, cancel), nil
}

func (d *initialDispatch) onSpawn(msg *message) (Dispatcher, error) {
	var (
		opts             Profile
		name, executable string
		args, env        map[string]string
	)

	if err := unpackArgs(d.ctx, msg.Args, &opts, &name, &executable, &args, &env); err != nil {
		return nil, err
	}

	isolateType := opts.Type()
	if isolateType == "" {
		return nil, fmt.Errorf("corrupted profile: %v", opts)
	}

	box, ok := getBoxes(d.ctx)[isolateType]
	if !ok {
		return nil, fmt.Errorf("isolate type %s is not available", isolateType)
	}

	pr, err := box.Spawn(d.ctx, opts, name, executable, args, env)
	if err != nil {
		GetLogger(d.ctx).Infof("initialDispatch.Handle.Spawn(): unable to spawn %v", err)
		return nil, err
	}

	go func() {
		for {
			select {
			case output := <-pr.Output():
				if output.Err != nil {
					reply(d.ctx, replySpawnError, [2]int{42, 42}, output.Err.Error())
				} else {
					reply(d.ctx, replySpawnWrite, output.Data)
				}
			case <-d.ctx.Done():
				return
			}
		}
	}()

	return newSpawnDispatch(d.ctx, pr), nil
}
