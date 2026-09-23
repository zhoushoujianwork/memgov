package core

func objectChange(kind, id string) []Change { return []Change{{ObjectType: kind, ObjectID: id}} }

type scanner interface{ Scan(...any) error }
