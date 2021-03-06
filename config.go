package maddy

import (
	"errors"
	"fmt"
	"os"

	"github.com/emersion/maddy/config"
	"github.com/emersion/maddy/log"
	"github.com/emersion/maddy/module"
)

/*
Config matchers for module interfaces.
*/

func authProvider(modName string) (module.AuthProvider, error) {
	modObj, err := module.GetInstance(modName)
	if err != nil {
		return nil, err
	}

	provider, ok := modObj.(module.AuthProvider)
	if !ok {
		return nil, fmt.Errorf("module %s doesn't implements auth. provider interface", modObj.Name())
	}
	return provider, nil
}

func authDirective(m *config.Map, node *config.Node) (interface{}, error) {
	if len(node.Args) != 1 {
		return nil, m.MatchErr("expected 1 argument")
	}
	if len(node.Children) != 0 {
		return nil, m.MatchErr("can't declare block here")
	}

	modObj, err := authProvider(node.Args[0])
	if err != nil {
		return nil, m.MatchErr("%s", err.Error())
	}

	return modObj, nil
}

func storageBackend(modName string) (module.Storage, error) {
	modObj, err := module.GetInstance(modName)
	if err != nil {
		return nil, err
	}

	backend, ok := modObj.(module.Storage)
	if !ok {
		return nil, fmt.Errorf("module %s doesn't implements storage interface", modObj.Name())
	}
	return backend, nil
}

func storageDirective(m *config.Map, node *config.Node) (interface{}, error) {
	if len(node.Args) != 1 {
		return nil, m.MatchErr("expected 1 argument")
	}
	if len(node.Children) != 0 {
		return nil, m.MatchErr("can't declare block here")
	}

	modObj, err := storageBackend(node.Args[0])
	if err != nil {
		return nil, m.MatchErr("%s", err.Error())
	}

	return modObj, nil
}

func deliveryTarget(modName string) (module.DeliveryTarget, error) {
	mod, err := module.GetInstance(modName)
	if mod == nil {
		return nil, err
	}

	target, ok := mod.(module.DeliveryTarget)
	if !ok {
		return nil, fmt.Errorf("module %s doesn't implements delivery target interface", mod.Name())
	}
	return target, nil
}

func deliverDirective(m *config.Map, node *config.Node) (interface{}, error) {
	if len(node.Args) != 1 {
		return nil, m.MatchErr("expected 1 argument")
	}
	if len(node.Children) != 0 {
		return nil, m.MatchErr("can't declare block here")
	}

	modObj, err := deliveryTarget(node.Args[0])
	if err != nil {
		return nil, m.MatchErr("%s", err.Error())
	}
	return modObj, nil
}

func logOutput(m *config.Map, node *config.Node) (interface{}, error) {
	if len(node.Args) == 0 {
		return nil, m.MatchErr("expected at least 1 argument")
	}
	if len(node.Children) != 0 {
		return nil, m.MatchErr("can't declare block here")
	}

	outs := make([]log.FuncLog, 0, len(node.Args))
	for _, arg := range node.Args {
		switch arg {
		case "stderr":
			outs = append(outs, log.StderrLog())
		case "syslog":
			syslogOut, err := log.Syslog()
			if err != nil {
				return nil, fmt.Errorf("failed to connect to syslog daemon: %v", err)
			}
			outs = append(outs, syslogOut)
		case "off":
			if len(node.Args) != 1 {
				return nil, errors.New("'off' can't be combined with other log targets")
			}
			return nil, nil
		default:
			w, err := os.OpenFile(arg, os.O_RDWR, os.ModePerm)
			if err != nil {
				return nil, fmt.Errorf("failed to create log file: %v", err)
			}

			outs = append(outs, log.WriterLog(w))
		}
	}

	if len(outs) == 1 {
		return outs[0], nil
	}
	return log.MultiLog(outs...), nil
}

func defaultLogOutput() (interface{}, error) {
	return log.StderrLog(), nil
}
