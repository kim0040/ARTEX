import { redirect } from "next/navigation";

export default function Home() {
  redirect("/function/tasks");
  return <>곧 공개됩니다.</>;
}
