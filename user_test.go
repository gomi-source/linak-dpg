package dpg

import "testing"

func TestUserOwnerBit(t *testing.T) {
	u := NewUser([]byte{0, 51, 251, 186})
	if u.Owner() {
		t.Fatal("a user ID starting with 0 is not the owner")
	}
	if u.ID != "0033fbba" {
		t.Errorf("ID = %q, want the hex of the whole user ID", u.ID)
	}

	u.SetOwner(true)
	if !u.Owner() {
		t.Fatal("SetOwner(true) should set the owner bit")
	}
	if u.Data[0] != 1 {
		t.Errorf("Data[0] = %d, want 1", u.Data[0])
	}
	if u.Data[1] != 51 {
		t.Errorf("SetOwner must not disturb the rest of the ID, Data[1] = %d", u.Data[1])
	}

	u.SetOwner(false)
	if u.Owner() {
		t.Fatal("SetOwner(false) should clear the owner bit")
	}
}

// A desk that answers with no payload must not crash the caller.
func TestUserOwnerBitOnEmptyData(t *testing.T) {
	u := NewUser(nil)
	if u.Owner() {
		t.Fatal("an empty user ID is not the owner")
	}
	u.SetOwner(true) // must not panic
	if u.Owner() {
		t.Fatal("there is no bit to set on an empty user ID")
	}
}
