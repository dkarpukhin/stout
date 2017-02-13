package porto

import (
	"io"
	"io/ioutil"
	"os"
	"syscall"
	"time"

	apexctx "github.com/m0sth8/context"
	"golang.org/x/net/context"

	porto "github.com/yandex/porto/src/api/go"
	portorpc "github.com/yandex/porto/src/api/go/rpc"
)

type container struct {
	ctx context.Context

	uuid           string
	containerID    string
	rootDir        string
	cleanupEnabled bool
	SetImgURI      bool

	volume       Volume
	extraVolumes []Volume
	output       io.Writer
}

// NOTE: is it better to have some kind of our own init inside Porto container to handle output?
func newContainer(ctx context.Context, portoConn porto.API, cfg containerConfig) (cnt *container, err error) {
	log := apexctx.GetLogger(ctx).WithField("container", cfg.ID)
	volume, err := cfg.CreateRootVolume(ctx, portoConn)
	if err != nil {
		log.WithError(err).Error("root volume construction failed")
		return nil, err
	}

	extravolumes, err := cfg.CreateExtraVolumes(ctx, portoConn, volume)
	if err != nil {
		log.WithError(err).Error("extra volumes construction failed")
		return nil, err
	}

	if err = cfg.CreateContainer(ctx, portoConn, volume, extravolumes); err != nil {
		volume.Destroy(ctx, portoConn)
		return nil, err
	}

	cnt = &container{
		ctx: ctx,

		uuid:           cfg.args["--uuid"],
		containerID:    cfg.ID,
		rootDir:        cfg.Root,
		cleanupEnabled: cfg.CleanupEnabled,
		SetImgURI:      cfg.SetImgURI,

		volume:       volume,
		extraVolumes: extravolumes,
		output:       ioutil.Discard,
	}
	return cnt, nil
}

func (c *container) start(portoConn porto.API, output io.Writer) (err error) {
	defer apexctx.GetLogger(c.ctx).WithField("id", c.containerID).Trace("start container").Stop(&err)
	c.output = output
	return portoConn.Start(c.containerID)
}

func (c *container) Kill() (err error) {
	defer apexctx.GetLogger(c.ctx).WithField("id", c.containerID).Trace("Kill container").Stop(&err)
	containersKilledCounter.Inc(1)
	portoConn, err := portoConnect()
	if err != nil {
		return err
	}
	defer portoConn.Close()
	defer c.Cleanup(portoConn)

	// After Kill the container must be in `dead` state
	// Wait seems redundant as we sent SIGKILL
	value, err := portoConn.GetData(c.containerID, "stdout")
	if err != nil {
		apexctx.GetLogger(c.ctx).WithField("id", c.containerID).WithError(err).Warn("unable to get stdout")
	}
	// TODO: add StringWriter interface to an output
	c.output.Write([]byte(value))
	apexctx.GetLogger(c.ctx).WithField("id", c.containerID).Infof("%d bytes of stdout have been sent", len(value))

	value, err = portoConn.GetData(c.containerID, "stderr")
	if err != nil {
		apexctx.GetLogger(c.ctx).WithField("id", c.containerID).WithError(err).Warn("unable to get stderr")
	}
	c.output.Write([]byte(value))
	apexctx.GetLogger(c.ctx).WithField("id", c.containerID).Infof("%d bytes of stderr have been sent", len(value))

	if err = portoConn.Kill(c.containerID, syscall.SIGKILL); err != nil {
		if !isEqualPortoError(err, portorpc.EError_InvalidState) {
			return err
		}
		return nil
	}

	if _, err = portoConn.Wait([]string{c.containerID}, 5*time.Second); err != nil {
		return err
	}

	return nil
}

func (c *container) Cleanup(portoConn porto.API) {
	if !c.cleanupEnabled {
		return
	}
	log := apexctx.GetLogger(c.ctx).WithField("id", c.containerID)

	var err error
	if err = c.volume.Destroy(c.ctx, portoConn); err != nil {
		log.WithError(err).Warn("root volume has not been destroyed")
	} else {
		log.Debug("root volume successfully destroyed")
	}

	for i, extraVolume := range c.extraVolumes {
		if err = extraVolume.Destroy(c.ctx, portoConn); err != nil {
			log.WithError(err).Warnf("extra volume %d has not been destroyed", i)
		} else {
			log.Debugf("extra volume %d successfully destroyed", i)
		}
	}
	if err = portoConn.Destroy(c.containerID); err != nil {
		log.WithError(err).Warn("Destroy error")
	} else {
		log.Debugf("Destroyed")
	}
	if err = os.RemoveAll(c.rootDir); err != nil {
		log.WithError(err).Warnf("Remove dirs %s", c.rootDir)
	} else {
		log.Debugf("Remove dirs %s successfully", c.rootDir)
	}
}
