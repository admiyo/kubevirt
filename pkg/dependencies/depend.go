package dependencies

import (
	"fmt"
	"reflect"
)

type ComponentFactory func(CC ComponentCache, which string) (interface{}, error)

type ComponentKey struct {
	Type  reflect.Type
	which string
}
type FactorySet struct {
	factories map[ComponentKey]ComponentFactory
}

type ComponentCache struct {
	factories  FactorySet
	components map[ComponentKey]interface{}
}

func NewFactorySet() FactorySet {
	fs := FactorySet{
		factories: make(map[ComponentKey]ComponentFactory),
	}
	return fs
}

func NewComponentCache(fs FactorySet) ComponentCache {
	cc := ComponentCache{
		components: make(map[ComponentKey]interface{}),
		factories:  fs}
	return cc
}

func (fs FactorySet) Register(Type reflect.Type, factory ComponentFactory) {
	var which string
	which = ""
	key := ComponentKey{Type, which}
	if _, ok := fs.factories[key]; ok {
		panic(fmt.Sprintf(
			"duplicate registration for type:%s which:%s", Type.String(), which))
	} else {
		fs.factories[key] = factory
	}
}

func (fs FactorySet) RegisterFactory(Type reflect.Type, which string, factory ComponentFactory) {
	key := ComponentKey{Type, which}
	if _, ok := fs.factories[key]; ok {
		panic("duplicate registration for type/which")
	} else {
		fs.factories[key] = factory
	}

}

func (cc ComponentCache) FetchComponent(Type reflect.Type, which string) interface{} {
	key := ComponentKey{Type, which}
	var err error
	if component, ok := cc.components[key]; ok {
		return component
	} else if factory, ok := cc.factories.factories[key]; ok {
		//IDEALLY locked on a per key basis.
		component, err = factory(cc, which)
		if err != nil {
			panic(err)
		}
		cc.components[key] = component
		return component
	} else {
		panic(component)
	}
}

func (cc ComponentCache) Fetch(Type reflect.Type) interface{} {
	return cc.FetchComponent(Type, "")
}
