export default function PageHead({ title }: { title: string }) {
  return (
    <div className="page-head">
      <h1>{title}</h1>
    </div>
  )
}
