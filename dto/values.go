package dto

import (
	"encoding/json"
	"strconv"
)

type IntValue int

func (i *IntValue) UnmarshalJSON(b []byte) error {
	var n int
	if err := json.Unmarshal(b, &n); err == nil {
		*i = IntValue(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return err
	}
	*i = IntValue(v)
	return nil
}

func (i IntValue) MarshalJSON() ([]byte, error) {
	return json.Marshal(int(i))
}

type BoolValue bool

func (b *BoolValue) UnmarshalJSON(data []byte) error {
	var boolean bool
	if err := json.Unmarshal(data, &boolean); err == nil {
		*b = BoolValue(boolean)
		return nil
	}
	var str string
	if err := json.Unmarshal(data, &str); err != nil {
		return err
	}
	if str == "true" {
		*b = BoolValue(true)
	} else if str == "false" {
		*b = BoolValue(false)
	} else {
		return json.Unmarshal(data, &boolean)
	}
	return nil
}
func (b BoolValue) MarshalJSON() ([]byte, error) {
	return json.Marshal(bool(b))
}

// StringValue 兼容 string 和 number 类型的 JSON 值，自动将数字转为字符串
type StringValue string

func (s *StringValue) UnmarshalJSON(b []byte) error {
	var str string
	if err := json.Unmarshal(b, &str); err == nil {
		*s = StringValue(str)
		return nil
	}
	// 传入的是数字，转为字符串
	var num json.Number
	if err := json.Unmarshal(b, &num); err != nil {
		return err
	}
	*s = StringValue(num.String())
	return nil
}

func (s StringValue) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(s))
}

func (s StringValue) String() string {
	return string(s)
}
