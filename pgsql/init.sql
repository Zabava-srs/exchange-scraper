create table exchange_scraped_data (
	id uuid default gen_random_uuid() primary key,
	datetime_query timestamp default now(),
    exchange varchar(64) not null,
	pair varchar(64) not null,
	price numeric not null
);