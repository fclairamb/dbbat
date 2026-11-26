import { useState } from "react";
import { useNavigate } from "@tanstack/react-router";
import { toast } from "sonner";

import {
  useCreateUserGroup,
  useUpdateUserGroup,
  useUsers,
  type UserGroup,
  type CreateUserGroupRequest,
} from "@/api";
import { FormPage } from "@/components/shared/FormPage";
import { MultiSelect } from "@/components/shared/MultiSelect";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

/**
 * Form for a user group: the `$uid` edit page renders it as a page, the create
 * dialog of the list wraps it in a Dialog.
 */
export function UserGroupForm({
  editing,
  onDone,
}: {
  editing: UserGroup | null;
  /** Create mode only: close the dialog the form is rendered in. */
  onDone?: () => void;
}) {
  const navigate = useNavigate();
  const { data: users = [] } = useUsers();

  const [name, setName] = useState(editing?.name ?? "");
  const [description, setDescription] = useState(editing?.description ?? "");
  const [memberUids, setMemberUids] = useState<string[]>(
    editing?.member_uids ?? []
  );

  const goBack = () => (onDone ? onDone() : navigate({ to: "/user-groups" }));

  const create = useCreateUserGroup({
    onSuccess: () => {
      toast.success("Group created");
      goBack();
    },
    onError: (e) => toast.error(e.message),
  });
  const update = useUpdateUserGroup({
    onSuccess: () => {
      toast.success("Group updated");
      goBack();
    },
    onError: (e) => toast.error(e.message),
  });

  const onSubmit = (e: React.FormEvent) => {
    e.preventDefault();

    const body: CreateUserGroupRequest = {
      name,
      description,
      member_uids: memberUids,
    };

    if (editing) {
      update.mutate({ uid: editing.uid, body });
    } else {
      create.mutate(body);
    }
  };

  return (
    <FormPage
      dialog={!editing}
      testId={editing ? "edit-user-group-page" : undefined}
      title={editing ? editing.name : "New User Group"}
      description="Groups let a grant definition apply to a subset of users instead of everyone."
      backTo="/user-groups"
      backLabel="User Groups"
      onSubmit={onSubmit}
      onCancel={goBack}
      submitLabel={editing ? "Save" : "Create"}
      submitTestId="user-group-submit"
      submitting={create.isPending || update.isPending}
    >
      <div className="max-w-2xl space-y-4">
        <div className="space-y-2">
          <Label htmlFor="group-name">Name</Label>
          <Input
            id="group-name"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="data-analysts"
            maxLength={64}
            required
            data-testid="user-group-name"
          />
        </div>
        <div className="space-y-2">
          <Label htmlFor="group-desc">Description</Label>
          <Input
            id="group-desc"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            placeholder="Analysts who self-serve read-only access"
            data-testid="user-group-description"
          />
        </div>
        <div className="space-y-2">
          <Label>Members</Label>
          <MultiSelect
            options={users.map((u) => ({ value: u.uid, label: u.username }))}
            selected={memberUids}
            onChange={setMemberUids}
            placeholder="No members"
            testId="user-group-members"
          />
        </div>
      </div>
    </FormPage>
  );
}
